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

// ---------------------------------------------------------------------------
// WIRE SESSIONS: the denylist (ADR-0075 Amendment 8, Johno 2026-09-03).
//
// A wire session is a real PostgreSQL session pinned to one backend for its
// whole life, and that backend is DISCARDED at close, never returned to the
// pool (closeSession). So the reason the pooled path refuses non-LOCAL SET —
// state persisting onto a connection the next user inherits — does not exist
// here: a session-level setting lives exactly as long as PostgreSQL says it
// does. Amendment 6 rule 1 therefore applies in full: no allowlist of
// settings. What remains is a SHORT DENYLIST naming only what must not change:
//
//   - the parsing-mode GUCs (parsingGUCs): they desynchronize the classifier
//     from the server's reading of the SQL, for every role, in every form;
//   - the engine's own belts (engineGUCs): a user must not be able to shorten
//     the server-side timeout the engine relies on;
//   - the non-GUC SET forms that are authority or transaction control in
//     disguise (SET ROLE, SET SESSION AUTHORIZATION, SET TRANSACTION, SET
//     SESSION CHARACTERISTICS): the first two would change WHO the target
//     thinks is running, escaping autodb's authorization; the last two are
//     transaction control, which the owned-control path handles and golib's
//     raw face poisons on;
//   - for READERS only, search_path (and its alias SET SCHEMA): the reader
//     analysis exempts catalog-named calls by name, and search_path is how a
//     user-defined function comes to answer to a catalog name.
//
// Startup parameters that name a GUC are admitted by EXACTLY this rule
// (OpenWireSessionWith): one implementation, so the startup packet and a SET
// statement cannot disagree about what a session may change.
// ---------------------------------------------------------------------------

// parsingGUCs are grammarGUCs minus search_path: the settings that change how
// the SERVER PARSES SQL. search_path changes name RESOLUTION, not parsing, and
// is a reader concern only (see readerGUCs).
var parsingGUCs = map[string]bool{
	"standard_conforming_strings": true,
	"backslash_quote":             true,
	"escape_string_warning":       true,
	"sql_mode":                    true,
	"autocommit":                  true,
	"transform_null_equals":       true,
}

// readerGUCs are refused for read-only wire sessions on top of the denylist:
// search_path and its alias SET SCHEMA (catalog-name shadowing), and the two
// GUC spellings that would lift the READ ONLY wrap by hand —
// transaction_read_only (this transaction) and default_transaction_read_only
// (every later one). SET TRANSACTION READ WRITE is the statement form of the
// same thing and is refused for everyone as transaction control.
var readerGUCs = map[string]bool{
	"search_path":                   true,
	"schema":                        true,
	"transaction_read_only":         true,
	"default_transaction_read_only": true,
}

// nonGUCSetForms are the leading names of SET statements that are not GUC
// assignments at all. parseSet reports them as the "name" it saw.
var nonGUCSetForms = map[string]string{
	"role":            "SET ROLE changes who the target believes is running; autodb's authorization is the identity",
	"authorization":   "SET SESSION AUTHORIZATION changes who the target believes is running; autodb's authorization is the identity",
	"transaction":     "SET TRANSACTION is transaction control; use BEGIN with its options",
	"characteristics": "SET SESSION CHARACTERISTICS is transaction control; use BEGIN with its options",
}

// ErrWireSetRefused reports a setting a wire session may never change.
// encodingGUCs carry the front door's no-transcoding invariant. The lease pins
// the encoding at acquisition — OpenWireSessionWith refuses a target that does
// not report server_encoding and client_encoding as UTF8 — BECAUSE autodb does
// not transcode. A session that could move client_encoding afterwards would
// break that contract for every row that followed, so it is refused in every
// spelling and for EVERY value: admitting TO 'UTF8' would be escape logic on an
// invariant, and a value-dependent gate is the hole it is written to close.
var encodingGUCs = map[string]bool{
	"client_encoding": true,
}

// settingAliases map SQL spellings that name the SAME setting onto the
// canonical GUC, so ONE denylist entry governs every spelling. PostgreSQL
// defines SET NAMES as an alias of SET client_encoding TO; parseSet and
// parseReset both report the leading name they saw ("names"), so without this
// a client_encoding entry would sit beside an unguarded alias.
var settingAliases = map[string]string{
	"names": "client_encoding",
}

// canonicalSetting resolves an alias spelling to the setting it actually names.
func canonicalSetting(name string) string {
	if canonical, ok := settingAliases[name]; ok {
		return canonical
	}
	return name
}

var ErrWireSetRefused = errors.New("exec: SET/RESET refused by the wire session-state gate")

// wireSettingDenied applies the denylist to a setting name for a wire session.
// It answers for SET and RESET alike, which is the point: one rule.
func wireSettingDenied(name string, readOnly bool) error {
	// Canonicalise BEFORE any lookup, so an alias and its GUC meet the same
	// entry. This is the one place both SET and RESET pass through.
	name = canonicalSetting(name)
	if why, ok := nonGUCSetForms[name]; ok {
		return fmt.Errorf("%w: %s", ErrWireSetRefused, why)
	}
	if engineGUCs[name] {
		return fmt.Errorf("%w: %s is set by the engine to bound this session's transactions and may not be "+
			"changed from the wire", ErrWireSetRefused, name)
	}
	if encodingGUCs[name] {
		return fmt.Errorf("%w: %s is pinned to UTF8 for the life of this session — the lease refuses a target "+
			"that does not report UTF8 because autodb does not transcode, so moving it afterwards would "+
			"break that contract for every row that followed; refused in every spelling and for every value",
			ErrWireSetRefused, name)
	}
	if parsingGUCs[name] {
		return fmt.Errorf("%w: %s changes how the server parses SQL and would desynchronize the engine's "+
			"reading of your statements from the server's; refused in every form", ErrWireSetRefused, name)
	}
	if readOnly && readerGUCs[name] {
		return fmt.Errorf("%w: a read-only session may not SET %s — it would move the read-only boundary "+
			"(transaction_read_only, default_transaction_read_only) or the catalog-name resolution "+
			"(search_path, SET SCHEMA) the reader analysis relies on", ErrWireSetRefused, name)
	}
	return nil
}

// admitWireSet decides whether a SET may run on a WIRE (pinned) session:
// anything not on the denylist, LOCAL or session-level. SET LOCAL still needs
// the client's transaction to be open, as on the pooled path — outside one
// there is nothing for LOCAL to be local to.
func admitWireSet(st setStatement, readOnly, txOpen bool) error {
	if err := wireSettingDenied(st.Name, readOnly); err != nil {
		return err
	}
	if st.Local && !txOpen {
		return fmt.Errorf("%w: %s reverts at the transaction boundary, and outside a transaction "+
			"there is no boundary for it to revert at", ErrSetOutsideTx, st.Name)
	}
	return nil
}

// resetStatement is a parsed RESET.
type resetStatement struct {
	// All reports RESET ALL.
	All bool
	// Name is the GUC, lower-cased ("" for RESET ALL).
	Name string
}

// parseReset reads the setting a RESET names. RESET SESSION AUTHORIZATION and
// RESET ROLE parse to "authorization" / "role" and meet the same denylist.
func parseReset(sqlText string) (resetStatement, error) {
	names, err := leadingNames(sqlText, 3)
	if err != nil {
		return resetStatement{}, err
	}
	if len(names) == 0 || !strings.EqualFold(names[0], "RESET") {
		return resetStatement{}, fmt.Errorf("%w: not a RESET statement", ErrStatementUnsupported)
	}
	rest := names[1:]
	if len(rest) == 0 {
		return resetStatement{}, fmt.Errorf("%w: RESET names no setting", ErrStatementUnsupported)
	}
	if strings.EqualFold(rest[0], "ALL") {
		return resetStatement{All: true}, nil
	}
	if strings.EqualFold(rest[0], "SESSION") && len(rest) > 1 {
		rest = rest[1:]
	}
	return resetStatement{Name: strings.ToLower(rest[0])}, nil
}

// admitWireReset decides whether a RESET may run on a wire session. RESET ALL
// is refused: it would reset the engine's own belts along with everything
// else, which no single-setting check can defend.
func admitWireReset(st resetStatement, readOnly bool) error {
	if st.All {
		return fmt.Errorf("%w: RESET ALL would reset the settings the engine relies on to bound this "+
			"session; reset settings by name", ErrWireSetRefused)
	}
	return wireSettingDenied(st.Name, readOnly)
}
