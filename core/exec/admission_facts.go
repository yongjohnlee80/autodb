package exec

import (
	"github.com/yongjohnlee80/autodb/core/admission"
)

// LegacyFacts carries the classifier's verdict for one statement, adapted
// to the admission seam's accessor shape. It is the phase-1 facts type:
// the classified Statement plus an optional parsed set-statement, so the
// statement guards and the GUC stages read one object. The phase-2
// analyzer's richer facts will implement the same interface.
//
// THE CONVERTED SLICES ARE BUILT ONCE, HERE. The accessors return cached
// admission-local slices rather than re-materialising them per call — a
// stage that asks twice, or several stages that each ask, must not
// reallocate on the per-statement hot path (every drive, every statement,
// including the extended path's per-frame work).
type LegacyFacts struct {
	stmt    Statement
	sqlText string
	setName string
	setLoc  bool
	setOK   bool
	textLen int

	mutations []admission.Mutation
	calls     []admission.Call
}

// NewLegacyFacts adapts a classified statement. setOK reports whether the
// caller parsed a set-statement shape for it (only the SET/LOCK arms do),
// with the setting's name and its LOCAL-ness. textLen is the statement
// text's length — the drive holds the text and the intake bound needs its
// size; the classifier's verdict carries the shape, not the bytes.
func NewLegacyFacts(stmt Statement, textLen int, setName string, setLocal, setOK bool) *LegacyFacts {
	return newLegacyFacts(stmt, "", textLen, setName, setLocal, setOK)
}

// NewLegacyFactsForText carries the raw SQL beside the verdict: the
// SET/RESET gates parse the text itself (their lexical shape is their
// own), and the drive holds it.
func NewLegacyFactsForText(stmt Statement, sqlText string, textLen int) *LegacyFacts {
	return newLegacyFacts(stmt, sqlText, textLen, "", false, false)
}

// newLegacyFacts constructs a LegacyFacts instance wrapping a parsed SQL statement.
func newLegacyFacts(stmt Statement, sqlText string, textLen int, setName string, setLocal, setOK bool) *LegacyFacts {
	lf := &LegacyFacts{stmt: stmt, sqlText: sqlText, setName: setName, setLoc: setLocal, setOK: setOK, textLen: textLen}
	// Convert ONCE (the performance decision from the design review): the
	// classifier's nested-mutation and call shapes become admission-local
	// slices here, at construction, and the accessors hand back the cached
	// values.
	for _, n := range stmt.Nested {
		lf.mutations = append(lf.mutations, admission.Mutation{
			Verb:     n.Verb,
			Depth:    n.Depth,
			HasWhere: n.HasWhere,
		})
	}
	for _, c := range stmt.Calls {
		lf.calls = append(lf.calls, admission.Call{Name: c.Name, Schema: c.Schema})
	}
	return lf
}

// Verb returns the top-level SQL verb of the statement.
// LegacyFacts implements admission.Facts.
func (l *LegacyFacts) Verb() string                    { return l.stmt.Verb }

// Class returns the admission fact classification of the statement.
func (l *LegacyFacts) Class() admission.FactClass      { return admission.FactClass(l.stmt.Class) }

// HasTopLevelWhere reports whether the top-level statement carries a WHERE clause.
func (l *LegacyFacts) HasTopLevelWhere() bool          { return l.stmt.HasTopLevelWhere }

// Mutations returns any nested data mutations within CTEs or subqueries.
func (l *LegacyFacts) Mutations() []admission.Mutation { return l.mutations }

// Calls returns all function/procedure invocations identified in the statement.
func (l *LegacyFacts) Calls() []admission.Call         { return l.calls }

// SetTarget returns configuration target parameters for SET commands.
func (l *LegacyFacts) SetTarget() (string, bool, bool) {
	return l.setName, l.setLoc, l.setOK
}

// TextLen is the statement text's length, for the intake bound.
func (l *LegacyFacts) TextLen() int { return l.textLen }
