package exec

import (
	"errors"
	"fmt"
	"strings"

	"github.com/yongjohnlee80/golib/dao"
)

// Transaction-control statements are STATE TRANSITIONS, not SQL to forward
// (ADR-0074 §3): the engine decides what a BEGIN means and calls the driver
// with an options value, and the user's control-verb string never reaches the
// wire. That promise is only honest if the options are actually read. A
// `BEGIN READ ONLY ISOLATION LEVEL SERIALIZABLE` that becomes a bare
// pool.Begin has silently given the caller a read-write, read-committed
// transaction and told them nothing — which is worse than refusing, because
// the caller believes the constraint holds.
//
// So every clause is either mapped or refused out loud. Nothing is discarded.

// TxAction is what a control statement does to the session's transaction.
type TxAction int

const (
	// TxBegin opens a transaction.
	TxBegin TxAction = iota + 1
	// TxCommit ends a transaction, keeping its work.
	TxCommit
	// TxRollback ends a transaction, discarding its work.
	TxRollback
)

// String implements fmt.Stringer.
func (a TxAction) String() string {
	switch a {
	case TxBegin:
		return "BEGIN"
	case TxCommit:
		return "COMMIT"
	case TxRollback:
		return "ROLLBACK"
	}
	return fmt.Sprintf("TxAction(%d)", int(a))
}

// TxControl is a parsed transaction-control statement: what the engine should
// do, and with which options. The verb is kept as written so a refusal or an
// audit record can quote the user rather than a normalization of them.
type TxControl struct {
	// Action is the state transition this statement asks for.
	Action TxAction
	// Verb is the statement's own leading keyword ("BEGIN", "START", "END", …).
	Verb string
	// Options are the driver transaction options, meaningful for TxBegin.
	// The zero value means server defaults, exactly as a bare BEGIN does.
	Options dao.TxOptions
	// Chain reports COMMIT/ROLLBACK AND CHAIN — end this transaction and
	// immediately open another with the same options. Parsing it is this
	// layer's job; whether the engine honors it is the session engine's.
	Chain bool
}

// ErrTxControlUnsupported reports a well-formed transaction-control clause
// this engine will not carry — a MySQL consistent-snapshot start, a savepoint
// target, a chain it has no session to chain into. It is deliberately
// distinct from a malformed statement: "autodb does not do that" and "that is
// not a thing" send a caller to different places.
var ErrTxControlUnsupported = errors.New("exec: unsupported transaction-control clause")

// ParseTxControl parses one transaction-control statement.
//
// Errors follow golib-dao-0017's order, because a caller's first question is
// always "did I write it wrong, or does this thing not do it?": malformed
// input is *dao.ErrTxOptionInvalid and is reported FIRST, a clause the engine
// cannot honor is ErrTxControlUnsupported, and an option the DRIVER cannot
// honor surfaces later as *dao.ErrTxOptionUnsupported from the driver itself.
// Only the first of those is a mistake the caller can fix by retyping.
func ParseTxControl(sqlText string) (TxControl, error) {
	// The leading verb is read FIRST, before the rest is tokenized. A
	// statement this parser was never meant to see — `SET LOCAL x = '5s'`,
	// `SELECT 1` — must be turned away as the wrong KIND of statement, not
	// described as a transaction-control statement with a malformed option.
	lead, err := leadingWord(sqlText)
	if err != nil {
		return TxControl{}, err
	}
	if lead == "" {
		return TxControl{}, ErrEmptyStatement
	}
	switch lead {
	case "BEGIN", "START", "COMMIT", "END", "ROLLBACK":
	default:
		return TxControl{}, fmt.Errorf("%w: %s is not a transaction-control statement",
			ErrStatementUnsupported, lead)
	}

	words, err := txWords(sqlText)
	if err != nil {
		return TxControl{}, err
	}
	if len(words) == 0 {
		return TxControl{}, ErrEmptyStatement
	}

	var tc TxControl
	tc.Verb = words[0]
	switch words[0] {
	case "BEGIN", "START":
		tc.Action = TxBegin
	case "COMMIT":
		tc.Action = TxCommit
	case "END":
		// END is COMMIT in PostgreSQL, and END is also the tail of a PL/pgSQL
		// block. Only the statement-leading form reaches here.
		tc.Action = TxCommit
	case "ROLLBACK":
		tc.Action = TxRollback
	default:
		return TxControl{}, fmt.Errorf("%w: %s is not a transaction-control statement",
			ErrStatementUnsupported, words[0])
	}

	rest := words[1:]
	// The noise word is optional in every dialect and carries no meaning.
	if len(rest) > 0 && (rest[0] == "WORK" || rest[0] == "TRANSACTION") {
		rest = rest[1:]
	}

	if tc.Action == TxBegin {
		unsupported, err := parseBeginOptions(rest, &tc)
		if err != nil {
			return TxControl{}, err
		}
		// The cross-field rules (DEFERRABLE only under SERIALIZABLE READ
		// ONLY, enum domains) belong to the DAO and are asked for by name
		// rather than restated here — one owner for the rule, one message.
		//
		// This runs BEFORE any deferred unsupported-clause refusal, because
		// golib-dao-0017 pins that order and it is the useful one: told first
		// that a clause is unsupported, a caller fixes their statement to
		// avoid it and hits the malformed-option error on the next attempt.
		if err := tc.Options.Validate(); err != nil {
			return TxControl{}, err
		}
		if unsupported != nil {
			return TxControl{}, unsupported
		}
		return tc, nil
	}
	if err := parseEndOptions(rest, &tc); err != nil {
		return TxControl{}, err
	}
	return tc, nil
}

// parseBeginOptions reads the option list of BEGIN / START TRANSACTION. It
// returns any unsupported-clause refusal SEPARATELY rather than raising it on
// the spot, so the caller can apply golib-dao-0017's ordering: malformed
// input is reported before a clause the engine cannot honor.
func parseBeginOptions(words []string, tc *TxControl) (unsupported error, err error) {
	var sawAccess, sawIso, sawDeferrable bool
	for i := 0; i < len(words); i++ {
		switch words[i] {
		case "ISOLATION":
			if i+2 >= len(words) || words[i+1] != "LEVEL" {
				return nil, invalidTx("ISOLATION", strings.Join(words[i:], " "), "expected ISOLATION LEVEL <level>")
			}
			lvl, consumed, err := parseIsolation(words[i+2:])
			if err != nil {
				return nil, err
			}
			if sawIso {
				return nil, invalidTx("Isolation", lvl.String(), "the isolation level is set more than once")
			}
			tc.Options.Isolation, sawIso = lvl, true
			i += 2 + consumed - 1

		case "READ":
			if i+1 >= len(words) {
				return nil, invalidTx("Access", "READ", "expected READ ONLY or READ WRITE")
			}
			var access dao.TxAccess
			switch words[i+1] {
			case "ONLY":
				access = dao.TxReadOnly
			case "WRITE":
				access = dao.TxReadWrite
			default:
				return nil, invalidTx("Access", "READ "+words[i+1], "expected READ ONLY or READ WRITE")
			}
			if sawAccess && tc.Options.Access != access {
				return nil, invalidTx("Access", "READ "+words[i+1], "the access mode is set twice, to conflicting values")
			}
			tc.Options.Access, sawAccess = access, true
			i++

		case "NOT":
			if i+1 >= len(words) || words[i+1] != "DEFERRABLE" {
				return nil, invalidTx("Deferrable", strings.Join(words[i:], " "), "expected NOT DEFERRABLE")
			}
			if sawDeferrable && tc.Options.Deferrable != dao.TxNotDeferrable {
				return nil, invalidTx("Deferrable", "not deferrable", "deferrability is set twice, to conflicting values")
			}
			tc.Options.Deferrable, sawDeferrable = dao.TxNotDeferrable, true
			i++

		case "DEFERRABLE":
			if sawDeferrable && tc.Options.Deferrable != dao.TxDeferrable {
				return nil, invalidTx("Deferrable", "deferrable", "deferrability is set twice, to conflicting values")
			}
			tc.Options.Deferrable, sawDeferrable = dao.TxDeferrable, true

		case "WITH":
			// MySQL's START TRANSACTION WITH CONSISTENT SNAPSHOT. Real,
			// well-formed, and unmappable onto dao.TxOptions — so it is
			// refused rather than dropped, because a caller who asked for a
			// consistent snapshot and silently did not get one is worse off
			// than one who was told no.
			if i+2 < len(words) && words[i+1] == "CONSISTENT" && words[i+2] == "SNAPSHOT" {
				unsupported = fmt.Errorf("%w: WITH CONSISTENT SNAPSHOT", ErrTxControlUnsupported)
				i += 2
				continue
			}
			return nil, invalidTx("option", strings.Join(words[i:], " "), "unrecognized transaction option")

		default:
			return nil, invalidTx("option", words[i], "unrecognized transaction option")
		}
	}
	return unsupported, nil
}

// parseIsolation reads a level name, returning how many words it consumed —
// two of the four are two words long, which is why it cannot be a map lookup.
func parseIsolation(words []string) (dao.TxIsolation, int, error) {
	if len(words) == 0 {
		return 0, 0, invalidTx("Isolation", "", "expected an isolation level")
	}
	switch words[0] {
	case "SERIALIZABLE":
		return dao.TxSerializable, 1, nil
	case "REPEATABLE":
		if len(words) > 1 && words[1] == "READ" {
			return dao.TxRepeatableRead, 2, nil
		}
		return 0, 0, invalidTx("Isolation", "REPEATABLE", "expected REPEATABLE READ")
	case "READ":
		if len(words) > 1 {
			switch words[1] {
			case "COMMITTED":
				return dao.TxReadCommitted, 2, nil
			case "UNCOMMITTED":
				return dao.TxReadUncommitted, 2, nil
			}
		}
		return 0, 0, invalidTx("Isolation", "READ "+join1(words[1:]), "expected READ COMMITTED or READ UNCOMMITTED")
	}
	return 0, 0, invalidTx("Isolation", words[0], "unrecognized isolation level")
}

// parseEndOptions reads the tail of COMMIT / ROLLBACK / END.
func parseEndOptions(words []string, tc *TxControl) error {
	if len(words) == 0 {
		return nil
	}
	// ROLLBACK TO [SAVEPOINT] name — a real statement, and one the engine has
	// no savepoint surface for. Refused by name so the message is about
	// savepoints rather than about a stray word.
	if tc.Action == TxRollback && words[0] == "TO" {
		return fmt.Errorf("%w: ROLLBACK TO SAVEPOINT (savepoints are not implemented)", ErrTxControlUnsupported)
	}
	if words[0] != "AND" {
		return invalidTx("option", strings.Join(words, " "), "expected AND [NO] CHAIN")
	}
	switch {
	case len(words) == 2 && words[1] == "CHAIN":
		tc.Chain = true
		return nil
	case len(words) == 3 && words[1] == "NO" && words[2] == "CHAIN":
		// The explicit spelling of the default. Recognized so it is not
		// mistaken for garbage, and it changes nothing.
		tc.Chain = false
		return nil
	}
	return invalidTx("option", strings.Join(words, " "), "expected AND CHAIN or AND NO CHAIN")
}

// invalidTx builds the malformed-input error, reusing the DAO's type so a
// caller has ONE error shape to handle across the parse and the driver call.
func invalidTx(option, value, reason string) error {
	return &dao.ErrTxOptionInvalid{Option: option, Value: value, Reason: reason}
}

func join1(words []string) string {
	if len(words) == 0 {
		return ""
	}
	return words[0]
}

// leadingWord returns the statement's first word, uppercased, skipping
// leading whitespace and comments. It reads only that far: deciding whether
// this parser owns the statement must not depend on the rest of it lexing.
func leadingWord(sqlText string) (string, error) {
	n := len(sqlText)
	for i := 0; i < n; {
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
				return "", fmt.Errorf("%w: unterminated block comment", ErrMalformedStatement)
			}
			i += 2 + end + 2
		case isWordStart(c):
			j := i + 1
			for j < n && isWordChar(sqlText[j]) {
				j++
			}
			return strings.ToUpper(sqlText[i:j]), nil
		default:
			return "", fmt.Errorf("%w: a statement cannot begin with %q",
				ErrStatementUnsupported, string(c))
		}
	}
	return "", nil
}

// txWords splits a control statement into uppercase words, dropping comments
// and one trailing semicolon.
//
// It refuses what it cannot mean rather than skipping it: a quote or a paren
// in a transaction-control statement is not a clause this parser silently
// ignores, it is a sign the input is not what the caller thinks it is.
func txWords(sqlText string) ([]string, error) {
	var out []string
	n := len(sqlText)
	for i := 0; i < n; {
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
		case c == ';':
			// One trailing semicolon is the statement's own terminator;
			// anything after it is a second statement, which this parser is
			// never handed.
			for j := i + 1; j < n; j++ {
				if !isSpace(sqlText[j]) {
					return nil, ErrMultiStatement
				}
			}
			i = n
		case isWordStart(c):
			j := i + 1
			for j < n && isWordChar(sqlText[j]) {
				j++
			}
			out = append(out, strings.ToUpper(sqlText[i:j]))
			i = j
		case c == ',':
			// PostgreSQL allows the option list to be comma-separated;
			// the separator carries no meaning of its own.
			i++
		default:
			return nil, invalidTx("statement", string(c),
				"a transaction-control statement has no quoted, parenthesized or operator text")
		}
	}
	return out, nil
}
