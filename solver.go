package sat

import (
	"fmt"

	"github.com/mitchellh/go-sat/cnf"
	"github.com/mitchellh/go-sat/packed"
)

type satResult byte

const (
	satResultUndef satResult = iota
	satResultUnsat
	satResultSat
)

// Solver is a SAT solver. This should be created with New to get
// the proper internal memory allocations. Using a manually allocated
// Solver will probably crash.
type Solver struct {
	// Trace, if set to true, will output trace debugging information
	// via the standard library `log` package. If true, Tracer must also
	// be set to a non-nil value. The easiest implmentation is a logger
	// created with log.NewLogger.
	Trace  bool
	Tracer Tracer

	// decideLiterals is to be set by tests to force a certain decision
	// literal ordering. This can be used to exercise specific solver
	// behavior being tested.
	decideLiterals []int

	//---------------------------------------------------------------
	// Internal fields, do not set
	//---------------------------------------------------------------
	result satResult

	f         cnf.Formula // formula we're solving
	m         *trail
	reasonMap map[cnf.Literal]cnf.Clause

	// problem
	clauses []packed.Clause  // clauses to solve
	vars    map[int]struct{} // list of available vars

	// trail
	assigns  map[int]Tribool // var assignments
	trail    []packed.Lit    // actual trail
	trailIdx []int           // indices of different decision levels in trail

	// conflict clause caching
	c  cnf.Clause
	cH map[cnf.Literal]struct{} // literals in C
	cP map[cnf.Literal]struct{} // literals in lower decision levels of C
	cL cnf.Literal              // last asserted literal in C
	cN int                      // number of literals in the highest decision level of C
}

// New creates a new solver and allocates the basics for it.
func New() *Solver {
	return &Solver{
		result: satResultUndef,

		m:         newTrail(),
		reasonMap: make(map[cnf.Literal]cnf.Clause),

		// problem
		vars: make(map[int]struct{}),

		// trail
		assigns: make(map[int]Tribool),
	}
}

// Solve finds a solution for the formula, returning true on satisfiability.
func (s *Solver) Solve() bool {
	if s.Trace {
		s.Tracer.Printf("[TRACE] sat: starting solve()")
	}

	// Check the result. This can be set already by a prior call to Solve
	// or via the AddClause process.
	if s.result != satResultUndef {
		if s.Trace {
			s.Tracer.Printf(
				"[TRACE] sat: result is already available: %s", s.result)
		}

		return s.result == satResultSat
	}

	for {
		// Perform unit propagation
		s.unitPropagate()

		conflictC := s.m.IsFormulaFalse(s.f)
		if !conflictC.IsZero() {
			if s.Trace {
				s.Tracer.Printf("[TRACE] sat: current trail contains negated formula: %s", s.m)
				s.Tracer.Printf("[TRACE] sat: conflict clause: %#v", conflictC)
			}

			// Set our conflict clause
			s.applyConflict(conflictC)

			// If we have no more decisions within the trail, then we've
			// failed finding a satisfying value.
			if s.m.DecisionsLen() == 0 {
				return false
			}

			// Explain to learn our conflict clause
			s.applyExplainUIP()
			if len(s.c) > 1 {
				if s.Trace {
					s.Tracer.Printf("[TRACE] sat: learned clause: %#v", s.c)
				}
				s.f = append(s.f, s.c)
			}
			s.applyBackjump()
		} else {
			// If the trail contains the same number of elements as
			// the variables in the formula, then we've found a satisfaction.
			if s.m.Len() == len(s.vars) {
				if s.Trace {
					s.Tracer.Printf("[TRACE] sat: solver found solution: %s", s.m)
				}

				return true
			}

			// Choose a literal to assert. For now we naively just select
			// the next literal.
			lit := s.selectLiteral()
			if s.Trace {
				s.Tracer.Printf("[TRACE] sat: assert: %d (decision)", lit)
			}
			s.assertLiteral(lit, true)
		}
	}

	return false
}

func (s *Solver) selectLiteral() cnf.Literal {
	tMap := map[cnf.Literal]struct{}{}
	for _, e := range s.m.elems {
		lit := e.Lit
		if lit < 0 {
			lit = cnf.Literal(-int(lit))
		}

		tMap[lit] = struct{}{}
	}

	if len(s.decideLiterals) > 0 {
		result := cnf.Literal(s.decideLiterals[0])
		s.decideLiterals = s.decideLiterals[1:]

		if _, ok := tMap[result]; ok {
			panic(fmt.Sprintf("decideLiteral taken: %d", result))
		}

		return result
	}

	for raw, _ := range s.vars {
		k := cnf.Literal(raw)
		if _, ok := tMap[k]; !ok {
			return k
		}
	}

	return cnf.Literal(0)
}

//-------------------------------------------------------------------
// Unit Propagation
//-------------------------------------------------------------------

func (s *Solver) unitPropagate() {
	for {
		for _, c := range s.f {
			for _, l := range c {
				if s.m.IsUnit(c, l) {
					if s.Trace {
						s.Tracer.Printf(
							"[TRACE] sat: found unit clause %v with literal %d in trail %s",
							c, l, s.m)
					}

					s.assertLiteral(l, false)
					s.reasonMap[l] = c
					goto UNIT_REPEAT
				}
			}
		}

		// We didn't find a unit clause, close it out
		return

	UNIT_REPEAT:
		// We found a unit clause but we have to check if we violated
		// constraints in the trail.
		if !s.m.IsFormulaFalse(s.f).IsZero() {
			return
		}
	}
}

//-------------------------------------------------------------------
// Conflict Clause Learning
//-------------------------------------------------------------------

func (s *Solver) applyConflict(c cnf.Clause) {
	// Build up our lookup caches for the conflict data to optimize
	// the conflict learning process.
	s.cH = make(map[cnf.Literal]struct{})
	s.cP = make(map[cnf.Literal]struct{})
	s.cN = 0
	for _, l := range c {
		s.addConflictLiteral(l)
	}

	// Find the last asserted literal using the cache
	for i := len(s.m.elems) - 1; i >= 0; i-- {
		s.cL = s.m.elems[i].Lit
		if _, ok := s.cH[s.cL.Negate()]; ok {
			break
		}
	}

	if s.Trace {
		s.Tracer.Printf(
			"[TRACE] sat: applyConflict. cH = %v; cP = %v; cL = %d; cN = %d",
			s.cH, s.cP, s.cL, s.cN)
	}
}

func (s *Solver) addConflictLiteral(l cnf.Literal) {
	if _, ok := s.cH[l]; !ok {
		level := s.m.Level(l.Negate())
		if level > 0 {
			s.cH[l] = struct{}{}
			if level == s.m.CurrentLevel() {
				s.cN++
			} else {
				s.cP[l] = struct{}{}
			}
		}
	}
}

func (s *Solver) removeConflictLiteral(l cnf.Literal) {
	delete(s.cH, l)

	if s.m.Level(l.Negate()) == s.m.CurrentLevel() {
		s.cN--
	} else {
		delete(s.cP, l)
	}
}

func (s *Solver) applyExplain(lit cnf.Literal) {
	s.removeConflictLiteral(lit.Negate())

	reason := s.reasonMap[lit]
	for _, l := range reason {
		if l != lit {
			s.addConflictLiteral(l)
		}
	}

	// Find the last asserted literal using the cache
	for i := len(s.m.elems) - 1; i >= 0; i-- {
		s.cL = s.m.elems[i].Lit
		if _, ok := s.cH[s.cL.Negate()]; ok {
			break
		}
	}

	if s.Trace {
		s.Tracer.Printf("[TRACE] sat: applyExplain: lit = %d, reason = %#v", lit, reason)
		s.Tracer.Printf(
			"[TRACE] sat: applyExplain. cH = %v; cP = %v; cL = %d; cN = %d",
			s.cH, s.cP, s.cL, s.cN)
	}
}

func (s *Solver) applyExplainUIP() {
	for s.cN != 1 { // !isUIP
		s.applyExplain(s.cL)
	}

	// buildC
	c := make([]cnf.Literal, 0, len(s.cP)+1)
	for l, _ := range s.cP {
		c = append(c, l)
	}
	c = append(c, s.cL.Negate())
	s.c = c
}

func (s *Solver) isUIP() bool {
	return s.cN == 1
}

//-------------------------------------------------------------------
// Backjumping
//-------------------------------------------------------------------

func (s *Solver) applyBackjump() {
	level := 0
	if len(s.cP) > 0 {
		for l, _ := range s.cP {
			if v := s.m.set[l.Negate()]; v > level {
				level = v
			}
		}
	}

	if s.Trace {
		s.Tracer.Printf(
			"[TRACE] sat: backjump. C = %#v; l = %d; level = %d",
			s.c, s.cL, level)
	}

	s.m.TrimToLevel(level)

	lit := s.cL.Negate()
	s.assertLiteral(lit, false)
	s.reasonMap[lit] = s.c

	if s.Trace {
		s.Tracer.Printf("[TRACE] sat: backjump. M = %s", s.m)
	}
}
