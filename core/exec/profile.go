package exec

import (
	"fmt"

	"github.com/yongjohnlee80/autodb/core/meta"
)

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
	// ProfileV1Compat is today's behavior, unchanged: control statements and
	// data-modifying CTEs are refused with the same errors they have always
	// been refused with. It is the default for every existing surface, and
	// the existing test suite is what pins it.
	ProfileV1Compat Profile = "v1compat"

	// ProfileSession is the session-capable profile (ADR-0074 §2). Today it
	// differs from v1compat in exactly one respect: it admits a
	// data-modifying CTE whose mutations are guarded, because the guard can
	// now see inside them (§6). Control verbs become engine actions when the
	// session engine lands (§3); until then it refuses them, and says why.
	ProfileSession Profile = "session"
)

// String implements fmt.Stringer.
func (p Profile) String() string { return string(p) }

// valid reports whether p is a profile this build knows.
func (p Profile) valid() bool { return p == ProfileV1Compat || p == ProfileSession }

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
		// The blanket refusal of data-modifying CTEs stays on this profile,
		// message included, even for the ones the guard could now clear
		// (ADR-0074 Amendment 3). The guard is genuinely fixed — see
		// guardWhere — but a statement a legacy surface has always refused
		// must not start executing a WRITE because a dependency was
		// upgraded. That surprise is the entire reason profiles exist.
		if len(st.Nested) > 0 {
			n := st.Nested[0]
			return fmt.Errorf("%w: data-modifying subquery/CTE (%s at nesting depth %d)",
				ErrStatementUnsupported, n.Verb, n.Depth)
		}
		return nil

	case ProfileSession:
		if st.Class == ClassControl {
			// Transaction control is now an engine action (ADR-0074 §3):
			// admitted here and performed as a state transition, never
			// forwarded as text.
			if txControlVerbs[st.Verb] {
				return nil
			}
			// The rest of the control verbs are still refused, and the
			// message says which kind of refusal it is. SET and LOCK have
			// admissible forms the gate matrix defines (SET LOCAL of an
			// allowlisted GUC inside a transaction, LOCK inside a
			// transaction) and arrive with that work; the others have no
			// admissible form at all. Collapsing the two into one message
			// would tell a caller to stop trying when they should wait, or
			// the reverse.
			if pendingControlVerbs[st.Verb] {
				return fmt.Errorf("%w: %s (this profile will admit it in a restricted form; that gate is not built yet)",
					ErrStatementUnsupported, st.Verb)
			}
			return fmt.Errorf("%w: %s (no admissible form on a session; it cannot be made safe on a pooled connection)",
				ErrStatementUnsupported, st.Verb)
		}
		// Data-modifying CTEs are admitted here and left to the guard, which
		// refuses the unguarded ones on their own merits — for every role
		// alike, since the guard is mistake-prevention and not authorization.
		return nil

	default:
		// Fail closed. An unknown profile is a configuration error, and the
		// safe reading of "I do not know what this surface may do" is
		// "nothing".
		return fmt.Errorf("%w: unknown capability profile %q", ErrStatementUnsupported, string(p))
	}
}

// txControlVerbs are the transaction boundaries the session profile performs
// as state transitions.
var txControlVerbs = map[string]bool{
	"BEGIN": true, "START": true, "COMMIT": true, "END": true, "ROLLBACK": true,
}

// pendingControlVerbs have an admissible form the gate matrix defines but
// which is not built yet — as opposed to the verbs that have none.
var pendingControlVerbs = map[string]bool{
	"SET": true, "LOCK": true, "CALL": true, "DO": true,
}

// profileFor resolves the capability profile for one connection (ADR-0074
// §2): the connection's own column, falling back to the engine's install-wide
// default when the row does not name one.
//
// An UNRECOGNIZED profile is not silently corrected to the default. It is
// kept, and Profile.admit refuses everything under it — a misconfigured
// connection must fail closed rather than quietly become the permissive one.
//
// The third source the ADR names, a per-user ceiling via grants, is not here
// yet: it narrows what an admitted profile may do rather than which profile
// applies, and it belongs with the grant work.
func (e *Engine) profileFor(row *meta.Connection) Profile {
	if row == nil || row.Profile == "" {
		return e.profile
	}
	return Profile(row.Profile)
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
