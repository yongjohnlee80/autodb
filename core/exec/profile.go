package exec

import "fmt"

// Capability profiles (ADR-0074 §2). The classifier is a pure lexer: it says
// what a statement IS. A profile says what the engine will RUN. Separating
// them is the point of engine v2 — the old design answered both questions in
// the tokenizer, so "is this a BEGIN?" and "may this caller open a
// transaction here?" had the same answer for everyone, forever, and a script
// containing BEGIN could not even be split.
//
// A profile is consulted in Engine.run, after classification and before
// authorization, because admission is a property of the surface the statement
// arrived through, not of the caller. Grants narrow what an admitted
// statement may do; they never widen what the profile admits.

// Profile names an engine capability profile.
type Profile string

const (
	// ProfileV1Compat is today's behavior, unchanged: control statements are
	// refused with the same error they have always been refused with. It is
	// the default for every existing surface, and the existing test suite is
	// what pins it.
	ProfileV1Compat Profile = "v1compat"
)

// String implements fmt.Stringer.
func (p Profile) String() string { return string(p) }

// valid reports whether p is a profile this build knows.
func (p Profile) valid() bool { return p == ProfileV1Compat }

// admit decides whether a classified statement may proceed under p. It is the
// ONLY place a control statement's admissibility is decided; the classifier
// no longer has an opinion, and neither does any call site.
//
// A profile that does not admit a statement says so with the same error
// identity the refusal has always had, because a caller's error handling is
// part of the compatibility surface — not just the fact of the refusal.
func (p Profile) admit(st Statement) error {
	switch p {
	case ProfileV1Compat:
		if st.Class == ClassControl {
			return fmt.Errorf("%w: %s (transaction control, session state, and PRAGMA have no safe meaning on pooled connections)",
				ErrStatementUnsupported, st.Verb)
		}
		return nil
	default:
		// Fail closed. An unknown profile is a configuration error, and the
		// safe reading of "I do not know what this surface may do" is
		// "nothing".
		return fmt.Errorf("%w: unknown capability profile %q", ErrStatementUnsupported, string(p))
	}
}

// guardWhere applies the WHERE guard to every mutation in a statement, at
// every depth (ADR-0074 §6).
//
// The guard's rule is one sentence — a mutation that can reach every row must
// say which rows it means — and the v1 implementation only ever applied it at
// paren depth 0. A mutation inside a CTE body is executed by PostgreSQL just
// the same, so `WITH x AS (DELETE FROM t RETURNING id) SELECT * FROM x`
// slipped past a guard that was looking in the wrong place. v1 papered over
// that by refusing data-modifying CTEs outright, which is a different rule
// with a different meaning: it refused the guarded ones too.
//
// Now the classifier reports each nested mutation with the WHERE found at ITS
// OWN depth, and the guard asks its one question of each of them. INSERT is
// unguarded here exactly as it is at top level — there is no full-table
// hazard to state a predicate against, and inventing a rule for it would be a
// new policy rather than the missing coverage.
func guardWhere(st Statement) error {
	if (st.Verb == "UPDATE" || st.Verb == "DELETE") && !st.HasTopLevelWhere {
		return ErrNoWhere
	}
	for _, n := range st.Nested {
		if n.Verb != "UPDATE" && n.Verb != "DELETE" {
			continue
		}
		if !n.HasWhere {
			return fmt.Errorf("%w: %s at nesting depth %d (a data-modifying CTE or subquery is guarded like any other mutation)",
				ErrNoWhere, n.Verb, n.Depth)
		}
	}
	return nil
}
