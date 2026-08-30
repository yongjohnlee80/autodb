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
// do, and with which options.
type TxControl struct {
	// Action is the state transition this statement asks for.
	Action TxAction
	// Verb is the statement's leading keyword, NORMALIZED to upper case
	// ("BEGIN", "START", "END", …). A refusal that wants to quote the user
	// verbatim has the original statement text to quote; this field is for
	// branching on, so it is normalized.
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

	toks, err := txTokens(sqlText)
	if err != nil {
		return TxControl{}, err
	}
	if len(toks) == 0 {
		return TxControl{}, ErrEmptyStatement
	}

	tc := TxControl{Verb: toks[0]}
	rest := toks[1:]

	// The prefix is verb-specific. PostgreSQL's BEGIN and the end controls
	// take an optional WORK or TRANSACTION; START REQUIRES TRANSACTION, and
	// bare `START` and `START WORK` are both syntax errors. Stripping one
	// optional noise word for every verb alike made those two parse.
	switch toks[0] {
	case "START":
		if len(rest) == 0 || rest[0] != "TRANSACTION" {
			return TxControl{}, invalidTx("START", joinToks(rest), "START requires TRANSACTION")
		}
		rest = rest[1:]
		tc.Action = TxBegin
	case "BEGIN":
		if len(rest) > 0 && (rest[0] == "WORK" || rest[0] == "TRANSACTION") {
			rest = rest[1:]
		}
		tc.Action = TxBegin
	case "COMMIT", "END":
		if len(rest) > 0 && (rest[0] == "WORK" || rest[0] == "TRANSACTION") {
			rest = rest[1:]
		}
		tc.Action = TxCommit
	case "ROLLBACK":
		if len(rest) > 0 && (rest[0] == "WORK" || rest[0] == "TRANSACTION") {
			rest = rest[1:]
		}
		tc.Action = TxRollback
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
//
// PostgreSQL's transaction_mode_list allows a comma between items and does
// not require one; it allows neither a leading nor a trailing comma, and
// never one inside an item. Dropping commas wherever they appeared made
// `BEGIN , READ ONLY`, `BEGIN READ, ONLY` and `BEGIN READ ONLY,` all parse —
// three statements the server rejects outright.
func parseBeginOptions(toks []string, tc *TxControl) (unsupported error, err error) {
	var sawAccess, sawIso, sawDeferrable bool
	i := 0
	for i < len(toks) {
		if toks[i] == "," {
			return nil, invalidTx("option", joinToks(toks), "a comma may only separate two transaction modes")
		}

		consumed, u, err := parseBeginOption(toks[i:], tc, &sawAccess, &sawIso, &sawDeferrable)
		if err != nil {
			return nil, err
		}
		if u != nil && unsupported == nil {
			unsupported = u
		}
		i += consumed

		// A comma here must be followed by another mode.
		if i < len(toks) && toks[i] == "," {
			i++
			if i >= len(toks) || toks[i] == "," {
				return nil, invalidTx("option", joinToks(toks), "a comma may only separate two transaction modes")
			}
		}
	}
	return unsupported, nil
}

// parseBeginOption reads exactly one transaction mode and reports how many
// tokens it consumed.
func parseBeginOption(toks []string, tc *TxControl, sawAccess, sawIso, sawDeferrable *bool) (int, error, error) {
	switch toks[0] {
	case "ISOLATION":
		if len(toks) < 3 || toks[1] != "LEVEL" {
			return 0, nil, invalidTx("ISOLATION", joinToks(toks), "expected ISOLATION LEVEL <level>")
		}
		lvl, consumed, err := parseIsolation(toks[2:])
		if err != nil {
			return 0, nil, err
		}
		if *sawIso && tc.Options.Isolation != lvl {
			return 0, nil, invalidTx("Isolation", lvl.String(), "the isolation level is set twice, to conflicting values")
		}
		tc.Options.Isolation, *sawIso = lvl, true
		return 2 + consumed, nil, nil

	case "READ":
		if len(toks) < 2 {
			return 0, nil, invalidTx("Access", "READ", "expected READ ONLY or READ WRITE")
		}
		var access dao.TxAccess
		switch toks[1] {
		case "ONLY":
			access = dao.TxReadOnly
		case "WRITE":
			access = dao.TxReadWrite
		default:
			return 0, nil, invalidTx("Access", "READ "+toks[1], "expected READ ONLY or READ WRITE")
		}
		if *sawAccess && tc.Options.Access != access {
			return 0, nil, invalidTx("Access", "READ "+toks[1], "the access mode is set twice, to conflicting values")
		}
		tc.Options.Access, *sawAccess = access, true
		return 2, nil, nil

	case "NOT":
		if len(toks) < 2 || toks[1] != "DEFERRABLE" {
			return 0, nil, invalidTx("Deferrable", joinToks(toks), "expected NOT DEFERRABLE")
		}
		if *sawDeferrable && tc.Options.Deferrable != dao.TxNotDeferrable {
			return 0, nil, invalidTx("Deferrable", "not deferrable", "deferrability is set twice, to conflicting values")
		}
		tc.Options.Deferrable, *sawDeferrable = dao.TxNotDeferrable, true
		return 2, nil, nil

	case "DEFERRABLE":
		if *sawDeferrable && tc.Options.Deferrable != dao.TxDeferrable {
			return 0, nil, invalidTx("Deferrable", "deferrable", "deferrability is set twice, to conflicting values")
		}
		tc.Options.Deferrable, *sawDeferrable = dao.TxDeferrable, true
		return 1, nil, nil

	case "WITH":
		// MySQL's START TRANSACTION WITH CONSISTENT SNAPSHOT. Real,
		// well-formed, and unmappable onto dao.TxOptions — so it is refused
		// rather than dropped, because a caller who asked for a consistent
		// snapshot and silently did not get one is worse off than one who
		// was told no.
		if len(toks) >= 3 && toks[1] == "CONSISTENT" && toks[2] == "SNAPSHOT" {
			return 3, fmt.Errorf("%w: WITH CONSISTENT SNAPSHOT", ErrTxControlUnsupported), nil
		}
		return 0, nil, invalidTx("option", joinToks(toks), "unrecognized transaction option")
	}
	return 0, nil, invalidTx("option", toks[0], "unrecognized transaction option")
}

// parseIsolation reads a level name, returning how many tokens it consumed —
// two of the four are two words long, which is why it cannot be a map lookup.
func parseIsolation(toks []string) (dao.TxIsolation, int, error) {
	if len(toks) == 0 {
		return 0, 0, invalidTx("Isolation", "", "expected an isolation level")
	}
	switch toks[0] {
	case "SERIALIZABLE":
		return dao.TxSerializable, 1, nil
	case "REPEATABLE":
		if len(toks) > 1 && toks[1] == "READ" {
			return dao.TxRepeatableRead, 2, nil
		}
		return 0, 0, invalidTx("Isolation", "REPEATABLE", "expected REPEATABLE READ")
	case "READ":
		if len(toks) > 1 {
			switch toks[1] {
			case "COMMITTED":
				return dao.TxReadCommitted, 2, nil
			case "UNCOMMITTED":
				return dao.TxReadUncommitted, 2, nil
			}
			return 0, 0, invalidTx("Isolation", "READ "+toks[1], "expected READ COMMITTED or READ UNCOMMITTED")
		}
		return 0, 0, invalidTx("Isolation", "READ", "expected READ COMMITTED or READ UNCOMMITTED")
	}
	return 0, 0, invalidTx("Isolation", toks[0], "unrecognized isolation level")
}

// parseEndOptions reads the tail of COMMIT / ROLLBACK / END. These take no
// comma-separated list at all, so any comma here is a syntax error.
func parseEndOptions(toks []string, tc *TxControl) error {
	if len(toks) == 0 {
		return nil
	}
	if toks[0] == "," {
		return invalidTx("option", joinToks(toks), "a transaction-ending statement takes no option list")
	}
	if tc.Action == TxRollback && toks[0] == "TO" {
		return parseSavepointTarget(toks)
	}
	if toks[0] != "AND" {
		return invalidTx("option", joinToks(toks), "expected AND [NO] CHAIN")
	}
	switch {
	case len(toks) == 2 && toks[1] == "CHAIN":
		tc.Chain = true
		return nil
	case len(toks) == 3 && toks[1] == "NO" && toks[2] == "CHAIN":
		// The explicit spelling of the default. Recognized so it is not
		// mistaken for garbage, and it changes nothing.
		tc.Chain = false
		return nil
	}
	return invalidTx("option", joinToks(toks), "expected AND CHAIN or AND NO CHAIN")
}

// parseSavepointTarget validates a complete `ROLLBACK TO [SAVEPOINT] name`
// before refusing it, so a malformed target is reported as malformed rather
// than as a capability this engine lacks — the order this parser promises.
//
// The keyword is genuinely optional and does not shadow a name: PostgreSQL
// reads `ROLLBACK TO SAVEPOINT` as a rollback to a savepoint CALLED
// "savepoint" (verified on 17.6 — it answers `savepoint "savepoint" does not
// exist`, and succeeds when one by that name is open). So a single word after
// TO is always the name, whatever it spells.
func parseSavepointTarget(toks []string) error {
	rest := toks[1:] // past TO
	var name string
	switch {
	case len(rest) == 1:
		name = rest[0]
	case len(rest) == 2 && rest[0] == "SAVEPOINT":
		name = rest[1]
	case len(rest) == 0:
		return invalidTx("ROLLBACK TO", "", "expected a savepoint name")
	default:
		return invalidTx("ROLLBACK TO", joinToks(rest), "expected ROLLBACK TO [SAVEPOINT] <name>")
	}
	_ = name // the target is well-formed; which one it names does not change the answer
	return fmt.Errorf("%w: ROLLBACK TO SAVEPOINT (savepoints are not implemented)",
		ErrTxControlUnsupported)
}

// invalidTx builds the malformed-input error, reusing the DAO's type so a
// caller has ONE error shape to handle across the parse and the driver call.
func invalidTx(option, value, reason string) error {
	return &dao.ErrTxOptionInvalid{Option: option, Value: value, Reason: reason}
}

func joinToks(toks []string) string { return strings.Join(toks, " ") }

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

// txTokens splits a control statement into upper-cased words and comma
// tokens, dropping comments and one trailing terminator.
//
// The comma is a TOKEN, not noise. Skipping it wherever it appeared meant the
// grammar could not tell `BEGIN ISOLATION LEVEL SERIALIZABLE, READ ONLY`
// (valid) from `BEGIN READ, ONLY` (a syntax error the server rejects).
//
// After the terminating `;` it accepts whitespace AND COMMENTS, and only
// another content token is a second statement. That is not a nicety: the
// splitter hands back each statement with its terminator and any trailing
// comment attached, so requiring bare whitespace made this parser unable to
// consume its own splitter's output — 153 of the 756 transaction controls in
// the LM deployment corpus, which is precisely the atomic-script workload.
//
// It refuses what it cannot mean rather than skipping it: a quote or a paren
// in a transaction-control statement is not a clause this parser silently
// ignores, it is a sign the input is not what the caller thinks it is.
func txTokens(sqlText string) ([]string, error) {
	var out []string
	terminated := false
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
			if terminated {
				return nil, ErrMultiStatement
			}
			terminated = true
			i++
		case isWordStart(c):
			if terminated {
				return nil, ErrMultiStatement
			}
			j := i + 1
			for j < n && isWordChar(sqlText[j]) {
				j++
			}
			out = append(out, strings.ToUpper(sqlText[i:j]))
			i = j
		case c == ',':
			if terminated {
				return nil, ErrMultiStatement
			}
			out = append(out, ",")
			i++
		default:
			if terminated {
				return nil, ErrMultiStatement
			}
			return nil, invalidTx("statement", string(c),
				"a transaction-control statement has no quoted, parenthesized or operator text")
		}
	}
	return out, nil
}
