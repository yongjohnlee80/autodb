package exec

import (
	"errors"
	"fmt"
	"strings"
)

// Session state: SET and LOCK (ADR-0074 §5, and G2 from the corpus survey).
//
// The gate has TWO axes here, not one. "SET" as a verb is too coarse to
// decide anything: whether a statement is admissible depends on its LOCALITY
// (LOCAL or not) and on WHICH GUC it names, and those two questions have
// different answers for different reasons.
//
//   - Locality, because a non-LOCAL SET persists on the underlying pooled
//     connection after the transaction ends and would leak to the next user
//     of that connection. SET LOCAL reverts at COMMIT/ROLLBACK by PostgreSQL
//     semantics, so on a pinned transaction nothing can outlive the pin.
//   - The GUC, because a handful of them change how the SERVER PARSES SQL.
//     Those desynchronize the classifier from the parser it is trying to
//     model, which attacks the audit invariant itself: the engine would be
//     recording a decision it made about a different language than the one
//     that ran. They are refused in every form, for every role, permanently
//     — LOCAL included.

// Session-state errors.
var (
	// ErrSetNotLocal reports a SET without LOCAL.
	ErrSetNotLocal = errors.New("exec: refused by session-state gate")

	// ErrSetGUCRefused reports a GUC that is never admissible.
	ErrSetGUCRefused = errors.New("exec: refused by session-state gate: that setting changes how SQL is parsed")

	// ErrSetOutsideTx reports SET LOCAL with no transaction open. Outside a
	// transaction there is nothing for LOCAL to be local to, so the setting
	// would either do nothing or persist — and which of those it is depends
	// on the server, which is not a thing to leave to chance.
	ErrSetOutsideTx = errors.New("exec: SET LOCAL needs an open transaction")

	// ErrLockOutsideTx reports LOCK with no transaction open. A lock taken
	// outside one is released immediately, so it is at best pointless and at
	// worst a caller believing they hold something they do not.
	ErrLockOutsideTx = errors.New("exec: LOCK needs an open transaction")
)

// grammarGUCs change how the server parses SQL. Refused in every form and
// for every role, including SET LOCAL, because the classifier's model of the
// language would stop matching the server's.
//
// This extends the existing DSN-level bans to the statement level: banning a
// setting in the connection string and then admitting it as a statement would
// be a gate with a door beside it.
var grammarGUCs = map[string]bool{
	"standard_conforming_strings": true,
	"backslash_quote":             true,
	"escape_string_warning":       true,
	"sql_mode":                    true,
	"autocommit":                  true,
	"transform_null_equals":       true,
	"search_path":                 true,
}

// benignGUCs are admissible as SET LOCAL inside a transaction.
//
// Deliberately short, and deliberately weighted to settings that make a
// session SAFER on a production target rather than more capable: a statement
// timeout and a lock timeout are the two an operator actually reaches for
// when running something against a live database, and they bound damage
// rather than enabling it. Widening this list is a decision with a reason,
// which is why it is a list rather than a rule.
var benignGUCs = map[string]bool{
	"lock_timeout":         true,
	"statement_timeout":    true,
	"deadlock_timeout":     true,
	"work_mem":             true,
	"maintenance_work_mem": true,
}

// engineGUCs are set by the ENGINE on a pinned transaction and are never
// admissible from a user statement.
//
// idle_in_transaction_session_timeout is the server-side belt, and letting a
// user set it inverts the very ordering the belt depends on: `SET LOCAL
// idle_in_transaction_session_timeout = '50ms'` inside a transaction lets the
// SERVER kill it before the engine's deadline fires, so the rollback happens
// with no audited engine record of why. It was on the benign allowlist —
// which made the gate the thing that undermined the guarantee it exists to
// protect.
//
// Engine-originated controls do not pass through the user gate at all. They
// are emitted directly by armServerBelt, which is the only correct
// relationship: a control the engine relies on must not be reachable through
// the surface it is guarding.
var engineGUCs = map[string]bool{
	"idle_in_transaction_session_timeout": true,
}

// setStatement is a parsed SET.
type setStatement struct {
	// Local reports SET LOCAL. SESSION and the bare form are both non-local.
	Local bool
	// Name is the GUC, lower-cased.
	Name string
}

// parseSet reads the locality and the GUC name from a SET statement.
//
// It is not a full parser and does not need to be: the gate's questions are
// "is it LOCAL" and "which setting", and everything after the name is the
// server's business. What it must not do is guess — an unparseable SET is
// refused rather than admitted on the assumption that it was probably fine.
func parseSet(sqlText string) (setStatement, error) {
	// A SET has its own lexical shape and cannot borrow the transaction
	// tokenizer, which refuses `=` and string literals — correctly, because
	// a transaction-control statement has neither. Here they are the normal
	// case, so this reads only the leading NAME tokens and stops at the
	// first thing that is not one, leaving the value to the server.
	names, err := leadingNames(sqlText, 3)
	if err != nil {
		return setStatement{}, err
	}
	if len(names) == 0 || !strings.EqualFold(names[0], "SET") {
		return setStatement{}, fmt.Errorf("%w: not a SET statement", ErrStatementUnsupported)
	}
	rest := names[1:]
	var out setStatement
	if len(rest) > 0 {
		switch {
		case strings.EqualFold(rest[0], "LOCAL"):
			out.Local = true
			rest = rest[1:]
		case strings.EqualFold(rest[0], "SESSION"):
			rest = rest[1:]
		}
	}
	if len(rest) == 0 {
		return setStatement{}, fmt.Errorf("%w: SET names no setting", ErrStatementUnsupported)
	}
	out.Name = strings.ToLower(rest[0])
	return out, nil
}

// leadingNames reads up to max leading identifier tokens — bare words or
// delimited identifiers — skipping whitespace and comments, and stopping at
// the first token that is neither.
//
// A delimited identifier keeps its case and its exact text, because quoting
// is how you say "this is a name"; a bare word is returned as written and
// normalized by the caller.
func leadingNames(sqlText string, max int) ([]string, error) {
	var out []string
	n := len(sqlText)
	for i := 0; i < n && len(out) < max; {
		c := sqlText[i]
		switch {
		case isSpace(c):
			i++
		case c == '-' && i+1 < n && sqlText[i+1] == '-':
			for i < n && sqlText[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && sqlText[i+1] == '*':
			end := strings.Index(sqlText[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("%w: unterminated block comment", ErrMalformedStatement)
			}
			i += 2 + end + 2
		case isWordStart(c):
			j := i + 1
			for j < n && isWordChar(sqlText[j]) {
				j++
			}
			out = append(out, sqlText[i:j])
			i = j
		case c == '"' || c == '`':
			name, j, err := scanDelimitedIdent(sqlText, i, c)
			if err != nil {
				return nil, err
			}
			out = append(out, name)
			i = j
		default:
			// `=`, `TO`'s value, a literal — the name is behind us.
			return out, nil
		}
	}
	return out, nil
}

// admitSet decides whether a SET may run.
//
// The order is deliberate. The GUC is checked BEFORE the locality, because a
// grammar-changing setting is refused in every form — telling someone to add
// LOCAL to a statement that would still be refused with LOCAL wastes their
// next attempt and implies the setting is otherwise available.
func admitSet(st setStatement, txOpen bool) error {
	if engineGUCs[st.Name] {
		return fmt.Errorf("%w: %s is set by the engine to bound this transaction, and letting a "+
			"statement change it would let the server end the transaction before the engine's own "+
			"deadline — with no audited record of why", ErrSetGUCRefused, st.Name)
	}
	if grammarGUCs[st.Name] {
		return fmt.Errorf("%w: %s desynchronizes the engine's reading of your SQL from the server's, "+
			"so it is refused in every form including SET LOCAL", ErrSetGUCRefused, st.Name)
	}
	if !st.Local {
		// The message Johno specified: say what would happen, and say what
		// to write instead.
		return fmt.Errorf("%w: SET without LOCAL persists on the underlying pooled connection beyond "+
			"your transaction and would leak to other users. HINT: use SET LOCAL %s = <value> inside a "+
			"transaction — it reverts automatically at COMMIT/ROLLBACK", ErrSetNotLocal, st.Name)
	}
	if !benignGUCs[st.Name] {
		return fmt.Errorf("%w: %s is not on the allowlist of settings this engine will carry",
			ErrSetGUCRefused, st.Name)
	}
	if !txOpen {
		return fmt.Errorf("%w: %s reverts at the transaction boundary, and outside a transaction "+
			"there is no boundary for it to revert at", ErrSetOutsideTx, st.Name)
	}
	return nil
}

// admitLock decides whether a LOCK may run (design doc G2).
//
// Only inside a transaction, where it auto-releases at the boundary. Outside
// one PostgreSQL releases it immediately, so admitting it there would leave a
// caller believing they hold a lock they do not — worse than refusing.
func admitLock(txOpen bool) error {
	if !txOpen {
		return fmt.Errorf("%w: a lock taken outside a transaction is released immediately, "+
			"so holding one requires BEGIN first", ErrLockOutsideTx)
	}
	return nil
}
