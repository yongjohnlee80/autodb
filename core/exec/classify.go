package exec

import (
	"errors"
	"fmt"
	"strings"

	"github.com/yongjohnlee80/autodb/core/engine"
)

// Class buckets a statement for authorization (maps 1:1 onto auth actions).
type Class string

const (
	// ClassRead is SELECT-shaped (reader and above).
	ClassRead Class = "read"
	// ClassWrite is DML (editor and above).
	ClassWrite Class = "write"
	// ClassDDL is schema/privilege change (editor and above, Objective 14).
	ClassDDL Class = "ddl"
	// ClassControl is a transaction/session control statement — BEGIN,
	// COMMIT, SET, LOCK and their kin. The lexer CLASSIFIES these;
	// it does not decide whether they may run. Admission belongs to the
	// engine's capability profile, because the answer depends on the profile,
	// the session's lifecycle state and the caller's grants — none of which a
	// tokenizer can see.
	ClassControl Class = "control"
)

func classRank(c Class) int {
	switch c {
	case ClassRead:
		return 1
	case ClassWrite:
		return 2
	case ClassDDL:
		return 3
	}
	// ClassControl deliberately ranks 0: it is not a point on the
	// read < write < ddl scale, and a control statement's class is decided by
	// its own verb rather than escalated to by anything it contains.
	return 0
}

var (
	// ErrEmptyStatement reports a blank (or comment-only) script.
	ErrEmptyStatement = errors.New("exec: empty statement")
	// ErrMultiStatement reports content after a top-level ';' — one
	// statement per execution.
	ErrMultiStatement = errors.New("exec: multiple statements are not allowed — execute one statement per call")
	// ErrStatementUnsupported reports a statement class the engine refuses:
	// transaction control, session state, PRAGMA, a data-modifying subquery/
	// CTE, or an unclassifiable leading token.
	ErrStatementUnsupported = errors.New("exec: statement is not supported through the execution engine")
	// ErrMalformedStatement reports an unterminated string/comment/quote.
	ErrMalformedStatement = errors.New("exec: malformed statement")
	// ErrNoWhere blocks UPDATE/DELETE without a top-level WHERE clause
	// (Objective 18).
	ErrNoWhere = errors.New("exec: UPDATE/DELETE without a WHERE clause is blocked")
	// ErrScriptTooLarge rejects a script exceeding the executable size cap
	// before it runs, so the audit record always equals what executed
	// (found in review).
	ErrScriptTooLarge = errors.New("exec: statement exceeds the maximum size")
)

// Statement is the classifier's verdict.
type Statement struct {
	// Verb is the classified main verb (uppercase), e.g. "SELECT", "DELETE".
	Verb string
	// Class is the authorization class: the MAXIMUM class of any verb in the
	// statement, so a read whose CTE body writes is authorized as a write.
	// Whether such a statement then EXECUTES is a profile decision, not this
	// field's — see Profile.admit: v1compat refuses data-modifying CTEs, the
	// session profile admits them guarded. The escalation is defense in
	// depth under both.
	//
	// The previous wording said they were "rejected outright — see Classify",
	// and the pointer was the worse half: it lands on the DDL-below-top-level
	// refusal, whose error text is word-for-word the one v1compat emits for a
	// data-modifying CTE, so a reader following it finds what looks like
	// confirmation. Nothing in Classify refuses a data-modifying DML CTE any
	// more; it records them in Nested and leaves the disposition to the
	// profile. Found and independently verified as the third copy
	// of one stale claim — the public README and convention R10 carried it
	// too — each one found by grepping outward from the last rather than by
	// review of the layer it lived in.
	Class Class
	// HasTopLevelWhere reports a WHERE at paren depth 0 after the main verb
	// (only meaningful for UPDATE/DELETE — the Objective 18 guard input).
	HasTopLevelWhere bool
	// Calls lists every function-call shape in the statement — an identifier
	// (unquoted, lowercased; or quoted, exact) immediately followed by '(' at any
	// depth, with its schema qualifier when written. It is the reader analysis
	// stage's input: the stage decides, against the
	// target's catalog, which of these are user-defined. Keywords that happen to
	// precede a paren (IN, VALUES, EXISTS …) appear here too; they are harmless,
	// because no user-defined function can be called by an unquoted keyword.
	Calls []FunctionCall
	// Nested lists the data-modifying verbs found BELOW top level — the
	// bodies of data-modifying CTEs and subqueries, which PostgreSQL really
	// does execute. HasTopLevelWhere is depth-0-only and says nothing about
	// them, which is the guard-coverage gap the design names: the v1
	// blanket refusal of data-modifying CTEs stood in for a guard that could
	// not see inside them. This is that guard's input.
	Nested []NestedMutation
}

// NestedMutation is one data-modifying verb found below top level, with the
// guard input for that verb at ITS OWN depth — a WHERE belonging to an inner
// subquery is not a guard on the mutation that encloses it.
type NestedMutation struct {
	// Verb is the mutating verb, uppercase (e.g. "DELETE").
	Verb string
	// Depth is the paren nesting depth the verb was found at.
	Depth int
	// HasWhere reports a WHERE at exactly that depth after the verb.
	HasWhere bool
}

// readVerbs are lexically read-only statements. PRAGMA is deliberately
// absent: SQLite has writable PRAGMA forms (e.g. `PRAGMA foreign_keys=OFF`),
// so PRAGMA is rejected as unsupported and introspection goes through the
// dedicated dao catalog API. SHOW /
// DESCRIBE are genuinely read-only.
var readVerbs = map[string]bool{
	"SELECT": true, "VALUES": true, "TABLE": true,
	"SHOW": true, "DESCRIBE": true, "DESC": true,
}

var writeVerbs = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "REPLACE": true, "MERGE": true,
}

var ddlVerbs = map[string]bool{
	"CREATE": true, "ALTER": true, "DROP": true, "TRUNCATE": true, "RENAME": true,
	"VACUUM": true, "REINDEX": true, "ANALYZE": true, "GRANT": true, "REVOKE": true,
	"COMMENT": true,
	// REFRESH MATERIALIZED VIEW. It was unclassifiable — and therefore
	// refused as an unknown verb — while running on a cron against the gold
	// database (design doc G5).
	"REFRESH": true,
}

// controlVerbs are transaction-control, session-state and cursor statements.
// They are CLASSIFIED, never rejected here: whether one may run
// is a question about the engine's capability profile and the session's
// lifecycle state, and the tokenizer knows neither. Under the v1compat
// profile the engine refuses every one of them, exactly as this list did when
// the refusal lived here.
var controlVerbs = map[string]bool{
	"BEGIN": true, "START": true, "COMMIT": true, "END": true, "ROLLBACK": true,
	"SAVEPOINT": true, "RELEASE": true, "SET": true, "RESET": true, "USE": true,
	"ATTACH": true, "DETACH": true, "LOCK": true, "UNLOCK": true,
	"CALL": true, "DO": true, "PREPARE": true, "EXECUTE": true,
	"DEALLOCATE": true, "DECLARE": true, "FETCH": true, "COPY": true,
	"PRAGMA": true,
}

var explainSkip = map[string]bool{
	"ANALYZE": true, "VERBOSE": true, "FORMAT": true, "JSON": true,
	"TREE": true, "TRADITIONAL": true, "QUERY": true, "PLAN": true,
}

func verbClass(word string) (Class, bool) {
	switch {
	case readVerbs[word]:
		return ClassRead, true
	case writeVerbs[word]:
		return ClassWrite, true
	case ddlVerbs[word]:
		return ClassDDL, true
	}
	return "", false
}

// FunctionCall is one `name(` shape found by the lexer. Name is lowercased for
// an unquoted identifier (PostgreSQL folds them) and exact for a quoted one;
// Schema is the qualifier before the dot when written, same normalization.
type FunctionCall struct {
	Schema string
	Name   string
	Quoted bool
}

// Classify tokenizes one SQL script and returns its statement verdict.
// backslashEscapes selects MySQL string semantics (backslash escapes inside
// quotes, '#' line comments, /*! executable comments); false selects
// PostgreSQL's, in which E'…' is a string whose backslashes escape.
//
// The engine's paths call classifyFor, never this: SQLite also answers false
// to BackslashEscapes, yet its comments do not nest, `$` opens a parameter
// rather than a string, and `[…]` is an identifier — so a bool cannot name
// its grammar.
//
// Classification Pipeline:
//
//	[Raw SQL String]
//	       │
//	       ▼
//	[Lexer / Scanner] (scanScript)
//	• Skip leading whitespace, comments (--, /* */)
//	• Tokenize keywords, identifiers, string literals, dollar quotes
//	• Track paren nesting depth
//	       │
//	       ├── Multiple statements? ──YES──> ErrMultiStatement
//	       ├── Unterminated token?  ──YES──> ErrMalformedStatement
//	       │
//	       ▼
//	[Extract Statement Properties]
//	• Main Verb (e.g. SELECT, INSERT, UPDATE, BEGIN)
//	• Scan CTEs / Subqueries for nested mutations (Verb, Depth, HasWhere)
//	• Scan Function Calls (Schema, Name, Quoted)
//	• Check Top-Level WHERE (for UPDATE/DELETE)
//	       │
//	       ▼
//	[Compute FactClass]
//	max(mainVerbClass, nestedVerbClasses)
//	       │
//	       ▼
//	Return Statement{Verb, Class, HasTopLevelWhere, Calls, Nested}
//
// The authorization class is the maximum verb class of the statement and of
// any statement body nested in it, so a read whose CTE writes is authorized
// as a write.
//
// A mutation in a CTE body — `WITH x AS (DELETE ...) SELECT ...`, which
// PostgreSQL really does execute — is CLASSIFIED here and reported in
// [Statement.Nested] with the WHERE found at its own depth. It is no longer
// rejected here: whether it may run is the engine profile's question, and
// whether it is guarded is the guard's. The blanket
// refusal it replaces stood in for a guard that could not see inside a CTE,
// and refused the guarded ones too.
func Classify(sqlText string, backslashEscapes bool) (Statement, error) {
	st, _, _, err := scanScript(sqlText, boolRules(backslashEscapes), false)
	return st, err
}

// classifyFor is Classify under the lexical rules of the named engine.
func classifyFor(sqlText string, eng engine.Name) (Statement, error) {
	st, _, _, err := scanScript(sqlText, rulesFor(eng), false)
	return st, err
}

// SplitStatements splits a script into its individual statements using the
// SAME lexer Classify uses — quoting, dollar-quoting, nested and MySQL
// executable comments, `--` disambiguation — because a splitter with its
// own idea of where a string ends is a security bug, not a convenience.
// Each returned statement is classified, authorized, WHERE-guarded and
// audited on its own when executed; splitting changes how much you can
// submit at once, never what the core will run.
//
// Like Classify, the bool names MySQL or PostgreSQL; the engine's paths call
// splitStatementsFor.
func SplitStatements(sqlText string, backslashEscapes bool) ([]string, error) {
	_, parts, _, err := scanScript(sqlText, boolRules(backslashEscapes), true)
	return parts, err
}

// splitStatementsFor is SplitStatements under the lexical rules of the named
// engine.
func splitStatementsFor(sqlText string, eng engine.Name) ([]string, error) {
	_, parts, _, err := scanScript(sqlText, rulesFor(eng), true)
	return parts, err
}

// stmtSpan is one statement's byte range in the original script, separators
// included, so a run of statements can be dispatched as the EXACT bytes the
// gate saw rather than a re-joined rendering of them.
type stmtSpan struct{ start, end int }

// splitStatementSpans is splitStatementsFor with each statement's byte range
// in the original text alongside its trimmed form. Same lexer, same
// boundaries.
func splitStatementSpans(sqlText string, eng engine.Name) ([]string, []stmtSpan, error) {
	_, parts, spans, err := scanScript(sqlText, rulesFor(eng), true)
	return parts, spans, err
}

// lexRules are the lexical rules a script is read by: each is one engine's
// answer to a question about where a token — and so a statement — ends. A
// rule the target does not have, applied anyway, ends a token somewhere the
// target does not, and the text after it is then read as something else.
type lexRules struct {
	backslashEscapes bool // MySQL strings, `#` comments, /*! … */, `--` needs a space
	escapeStrings    bool // E'…' strings in which a backslash escapes
	dollarQuotes     bool // $$…$$ and $tag$…$tag$ strings
	nestedComments   bool // /* … /* … */ … */ nests
	bracketIdents    bool // […] is an identifier running to the first ]
	tclParameters    bool // $name, @name, :name, #name, with a (…) suffix
}

// rulesFor is the named engine's lexical rules.
func rulesFor(eng engine.Name) lexRules {
	return lexRules{
		backslashEscapes: eng.BackslashEscapes(),
		escapeStrings:    eng.EscapeStringConstants(),
		dollarQuotes:     eng.DollarQuotedStrings(),
		nestedComments:   eng.NestedBlockComments(),
		bracketIdents:    eng.BracketQuotedIdentifiers(),
		tclParameters:    eng.TclVariables(),
	}
}

// boolRules is what the bool forms name: true MySQL, false PostgreSQL.
func boolRules(backslashEscapes bool) lexRules {
	if backslashEscapes {
		return rulesFor(engine.MySQL)
	}
	return rulesFor(engine.Postgres)
}

// scanScript is the one tokenizer. With split=false it enforces the
// one-statement rule (ErrMultiStatement) and returns the statement's
// verdict; with split=true it records statement boundaries instead and
// returns their texts.
func scanScript(sqlText string, lex lexRules, split bool) (Statement, []string, []stmtSpan, error) {
	var st Statement
	var parts []string
	var spans []stmtSpan
	var calls []FunctionCall
	// Call tracking: the most recent token, when it is an identifier, is the
	// callee candidate for a '(' that follows it. Whitespace and comments do not
	// disturb it; every other token clears it.
	var (
		identLive   bool   // the last token was an identifier
		identRaw    string // its text (inner text for a quoted identifier)
		identQuoted bool
		qualRaw     string // schema qualifier collected across a '.'
		qualQuoted  bool
		qualPending bool // a '.' followed the previous identifier
	)
	stmtStart := 0
	var (
		depth       int
		searching   = true // still looking for the main verb
		inExplain   bool
		inWith      bool
		ended       bool // a top-level ';' was consumed
		sawContent  bool
		execComment bool // inside a MySQL /*! ... */ executable comment
	)
	// parenCtx tracks, per open paren, whether it opened a STATEMENT body —
	// `... AS ( SELECT/DELETE/... )` — and whether its first word has been
	// seen yet. Without this the scanner reads any word that happens to spell
	// a verb as a verb, wherever it appears, so a column named `comment` in a
	// CREATE TABLE body reads as DDL nested inside a statement, and
	// `SELECT comment FROM notes` classifies as ddl and is refused to the
	// readers who are granted it. A verb only means a verb in verb position.
	type parenState struct {
		body    bool // opened directly after AS — a CTE or subquery body
		sawWord bool // its first word token has been consumed
	}
	var parens []parenState
	// The last three words, most recent first. A CTE body is introduced by
	// AS, and since PostgreSQL 12 also by AS MATERIALIZED and AS NOT
	// MATERIALIZED — a hint that changes planning, not the fact that the
	// parens hold a statement.
	var w1, w2, w3 string
	var nested []NestedMutation
	maxClass := Class("")
	escalate := func(c Class) {
		if classRank(c) > classRank(maxClass) {
			maxClass = c
		}
	}
	n := len(sqlText)
	i := 0

	content := func() error {
		if ended {
			if !split {
				return ErrMultiStatement
			}
			// A new statement begins here: bank the previous one and
			// reset the per-statement classification state.
			if part := strings.TrimSpace(sqlText[stmtStart:i]); part != "" {
				parts = append(parts, part)
				spans = append(spans, stmtSpan{start: stmtStart, end: i})
			}
			stmtStart = i
			ended, searching, inWith, inExplain = false, true, false, false
			maxClass = ""
			nested = nil
			parens = parens[:0]
			w1, w2, w3 = "", "", ""
			st = Statement{}
		}
		sawContent = true
		return nil
	}

	for i < n {
		c := sqlText[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f':
			i++

		case c == '-' && i+1 < n && sqlText[i+1] == '-':
			// MySQL requires the double-dash comment to be followed by
			// whitespace (or EOL); `--x` is the subtraction operator, not a
			// comment. Postgres/sqlite treat any `--` as a comment. Modeling
			// the target's rule prevents a `--`-hidden tail on MySQL from
			// being wrongly commented out.
			if lex.backslashEscapes && i+2 < n && !isSpace(sqlText[i+2]) {
				if err := content(); err != nil {
					return st, parts, spans, err
				}
				i++ // consume one '-' as an operator; keep scanning
				continue
			}
			for i < n && sqlText[i] != '\n' {
				i++
			}

		case c == '#' && lex.backslashEscapes:
			for i < n && sqlText[i] != '\n' {
				i++
			}

		case c == '/' && i+1 < n && sqlText[i+1] == '*':
			// MySQL executable comment /*![digits] ... */: the server RUNS
			// the body, so its tokens are live, not commented out. Skip only
			// the opener + optional version digits; the matching */ is
			// handled as whitespace below.
			if lex.backslashEscapes && i+2 < n && sqlText[i+2] == '!' {
				j := i + 3
				for j < n && sqlText[j] >= '0' && sqlText[j] <= '9' {
					j++
				}
				execComment = true
				i = j
				continue
			}
			// PostgreSQL nests block comments; MySQL and SQLite do not, and
			// there the FIRST */ ends the comment. Model the target: read as
			// nesting, SQLite's `SELECT 1 /* /* */ ; DROP TABLE t; -- */` was
			// one statement here — the comment swallowing the DROP — and two
			// on the server, which runs every statement a call carries.
			nests := lex.nestedComments
			level, j := 1, i+2
			for j < n && level > 0 {
				switch {
				case nests && sqlText[j] == '/' && j+1 < n && sqlText[j+1] == '*':
					level++
					j += 2
				case sqlText[j] == '*' && j+1 < n && sqlText[j+1] == '/':
					level--
					j += 2
				default:
					j++
				}
			}
			if level > 0 {
				return st, parts, spans, fmt.Errorf("%w: unterminated block comment", ErrMalformedStatement)
			}
			i = j

		case c == '*' && i+1 < n && sqlText[i+1] == '/' && execComment:
			execComment = false
			i += 2

		case c == '\'' || c == '"' || c == '`':
			if err := content(); err != nil {
				return st, parts, spans, err
			}
			j, err := scanQuoted(sqlText, i, c, lex.backslashEscapes)
			if err != nil {
				return st, parts, spans, err
			}
			if c == '"' && j-i >= 2 {
				if qualPending {
					qualRaw, qualQuoted, qualPending = identRaw, identQuoted, false
				} else {
					qualRaw, qualQuoted = "", false
				}
				identRaw, identQuoted, identLive = sqlText[i+1:j-1], true, true
			} else {
				identLive, qualPending = false, false
			}
			// A quoted string or delimited identifier is a token, so it ends
			// the run of words before it. Without this, `AS "x" (comment)` —
			// a perfectly ordinary PostgreSQL column-alias list — still saw
			// AS as the nearest word when the paren opened, and the alias
			// list was read as a statement body.
			w1, w2, w3 = "", "", ""
			i = j

		case lex.escapeStrings && (c == 'E' || c == 'e') && i+1 < n && sqlText[i+1] == '\'' &&
			(i == 0 || !isWordChar(sqlText[i-1])):
			// A PostgreSQL escape string. Inside E'…' a backslash escapes the
			// next byte, so E'\'' is the one-character string "'" and the
			// literal goes on; an ordinary '…' keeps the standard reading, in
			// which a backslash is a plain byte. Read as a word and a standard
			// string, `SELECT E'\''; CREATE TABLE t (x int); --'` was one
			// statement here and two on the server.
			//
			// Only an E that begins a token opens one: the e that ends
			// `name'…'` or follows the digits of `1e'…'` does not. The E is
			// the string's first byte, not a word, so it is handled exactly as
			// the quote case handles a string: one token, ending the run of
			// words and any callee candidate before it.
			if err := content(); err != nil {
				return st, parts, spans, err
			}
			j, err := scanQuoted(sqlText, i+1, '\'', true)
			if err != nil {
				return st, parts, spans, err
			}
			identLive, qualPending = false, false
			w1, w2, w3 = "", "", ""
			i = j

		case c == '$' && lex.dollarQuotes:
			if err := content(); err != nil {
				return st, parts, spans, err
			}
			if j, ok, err := scanDollarQuote(sqlText, i); err != nil {
				return st, parts, spans, err
			} else if ok {
				i = j
			} else {
				i++ // '$1' positional parameter etc.
			}
			w1, w2, w3 = "", "", ""
			identLive, qualPending = false, false

		case lex.tclParameters && (c == '$' || c == '@' || c == ':' || c == '#'):
			// A SQLite parameter. Its name may end in a (…) suffix that runs
			// to the first ) with nothing escaped inside it, so a quote, a
			// comment opener or a semicolon there is a byte of the parameter.
			// Read as an operator and a word, `SELECT $a(x'y); DROP TABLE t;
			// --'` was one statement here, its quote opening a string that ran
			// to the end, and two on the server. And since `$` is a name
			// character, `$$x` is a parameter too: SQLite has no dollar-quoted
			// strings, and `$$x;y$$` is two statements there.
			if err := content(); err != nil {
				return st, parts, spans, err
			}
			j, err := scanTclParameter(sqlText, i)
			if err != nil {
				return st, parts, spans, err
			}
			w1, w2, w3 = "", "", ""
			identLive, qualPending = false, false
			i = j

		case c == '[' && lex.bracketIdents:
			// A SQLite delimited identifier, which runs to the FIRST ] with
			// no escape inside it. Read as a lone [, `SELECT 1 AS [a'b];
			// DROP TABLE t; --'` was one statement here — the quote opening a
			// string that ran to the end — and two on the server; and the
			// semicolon in `[a;b]` is a byte of a name, not a boundary.
			if err := content(); err != nil {
				return st, parts, spans, err
			}
			end := strings.IndexByte(sqlText[i+1:], ']')
			if end < 0 {
				return st, parts, spans, fmt.Errorf("%w: unterminated \"[\"-quoted identifier", ErrMalformedStatement)
			}
			w1, w2, w3 = "", "", ""
			identLive, qualPending = false, false
			i += end + 2

		case c == '(':
			if err := content(); err != nil {
				return st, parts, spans, err
			}
			if identLive {
				fc := FunctionCall{Name: identRaw, Quoted: identQuoted}
				if !identQuoted {
					fc.Name = strings.ToLower(fc.Name)
				}
				if qualRaw != "" {
					fc.Schema = qualRaw
					if !qualQuoted {
						fc.Schema = strings.ToLower(fc.Schema)
					}
				}
				calls = append(calls, fc)
			}
			identLive, qualPending, qualRaw = false, false, ""
			parens = append(parens, parenState{body: opensStatementBody(w1, w2, w3)})
			w1, w2, w3 = "", "", ""
			depth++
			i++

		case c == ')':
			if err := content(); err != nil {
				return st, parts, spans, err
			}
			if depth > 0 {
				depth--
			}
			if len(parens) > 0 {
				parens = parens[:len(parens)-1]
			}
			w1, w2, w3 = "", "", ""
			identLive, qualPending = false, false
			i++

		case c == ';':
			if depth == 0 {
				ended = true
			}
			w1, w2, w3 = "", "", ""
			identLive, qualPending = false, false
			i++

		case c == '.':
			qualPending = identLive
			identLive = false
			i++
		case isWordStart(c):
			if err := content(); err != nil {
				return st, parts, spans, err
			}
			j := i + 1
			for j < n && isWordChar(sqlText[j]) {
				j++
			}
			word := strings.ToUpper(sqlText[i:j])
			if qualPending {
				qualRaw, qualQuoted, qualPending = identRaw, identQuoted, false
			} else {
				qualRaw, qualQuoted = "", false
			}
			identRaw, identQuoted, identLive = sqlText[i:j], false, true
			i = j

			// EXPLAIN option words (ANALYZE, VERBOSE, FORMAT …) are not
			// verbs here — ANALYZE is also DDL, and options may sit inside
			// parens: `EXPLAIN (ANALYZE, BUFFERS) SELECT`. Skip them before
			// any class escalation or depth check.
			if inExplain && searching && explainSkip[word] {
				continue
			}

			// Verb position. At top level that is the main verb the scanner
			// is still searching for; below top level it is the first word
			// of a statement body — the `DELETE` in `WITH x AS (DELETE …)`.
			// Anywhere else a word that spells a verb is an identifier: a
			// column, an alias, a table. Classifying those as verbs is how
			// `SELECT comment FROM notes` became ddl and how a table with a
			// `comment` column became unclassifiable.
			// A paren introduced by AS holds a STATEMENT; every other paren
			// holds a list or an expression. That is the whole discriminator,
			// and it has to be the whole discriminator: treating the first
			// word of ANY paren as a verb is what made `SELECT (comment)` and
			// `INSERT INTO t (comment, id)` classify as ddl, so the
			// identifier bug survived wherever the offending column happened
			// to come first.
			statementBody := depth > 0 && len(parens) > 0 &&
				parens[len(parens)-1].body && !parens[len(parens)-1].sawWord
			verbPosition := (depth == 0 && searching) || statementBody
			if depth > 0 && len(parens) > 0 {
				parens[len(parens)-1].sawWord = true
			}
			w1, w2, w3 = word, w1, w2

			if cls, ok := verbClass(word); ok && verbPosition {
				// DDL below top level is not valid SQL in any target dialect;
				// it stays refused outright rather than being handed to a
				// guard that has no rule for it.
				if statementBody && cls == ClassDDL {
					return st, parts, spans, fmt.Errorf("%w: data-modifying subquery/CTE (%s at nesting depth %d)", ErrStatementUnsupported, word, depth)
				}
				// A mutation below top level is recorded WITH ITS DEPTH, so
				// the guard can ask whether that particular mutation is
				// guarded rather than whether the statement as a whole
				// happens to contain a WHERE somewhere.
				// Only a statement body yields a nested MUTATION — and only a
				// statement body is read as a verb at all, so this is where
				// every below-top-level verb is accounted for.
				if statementBody && cls == ClassWrite {
					nested = append(nested, NestedMutation{Verb: word, Depth: depth})
				}
				escalate(cls)
			}

			if depth != 0 {
				// A WHERE below top level guards the innermost mutation open
				// at exactly this depth — never one at a shallower depth,
				// whose own predicate this is not.
				if word == "WHERE" {
					for k := len(nested) - 1; k >= 0; k-- {
						if nested[k].Depth == depth {
							nested[k].HasWhere = true
							break
						}
					}
				}
				continue
			}
			if searching {
				switch {
				case word == "EXPLAIN":
					inExplain = true
				case inExplain && explainSkip[word]:
					// EXPLAIN options — keep scanning for the inner verb.
				case word == "WITH":
					inWith = true
				default:
					if _, ok := verbClass(word); ok {
						st.Verb = word
						searching = false
						continue
					}
					if inWith {
						continue // CTE names, AS, RECURSIVE, column lists
					}
					if controlVerbs[word] {
						// Classified, not refused. Whether it may run is the
						// engine profile's question, and
						// answering it here also broke SplitStatements: a
						// script containing BEGIN could not even be split,
						// because the splitter shares this scanner.
						st.Verb = word
						searching = false
						continue
					}
					return st, parts, spans, fmt.Errorf("%w: %s", ErrStatementUnsupported, word)
				}
				continue
			}
			if word == "WHERE" {
				st.HasTopLevelWhere = true
			}

		default:
			if err := content(); err != nil {
				return st, parts, spans, err
			}
			// Commas, operators, numbers — any token that is not a word ends
			// the run of words. Only whitespace and comments are transparent
			// to AS adjacency.
			w1, w2, w3 = "", "", ""
			identLive, qualPending = false, false
			i++
		}
	}

	if execComment {
		return st, parts, spans, fmt.Errorf("%w: unterminated executable comment", ErrMalformedStatement)
	}
	if !sawContent {
		return st, parts, spans, ErrEmptyStatement
	}
	st.Calls = calls
	if split {
		if part := strings.TrimSpace(sqlText[stmtStart:]); part != "" {
			parts = append(parts, part)
			spans = append(spans, stmtSpan{start: stmtStart, end: len(sqlText)})
		}
		return st, parts, spans, nil
	}
	if searching {
		return st, parts, spans, fmt.Errorf("%w: unable to classify the statement", ErrStatementUnsupported)
	}
	st.Nested = nested
	// A control statement's class is its own verb's, never something it
	// contains: `LOCK TABLE t` must not read as ClassRead because TABLE is a
	// read verb.
	if controlVerbs[st.Verb] {
		st.Class = ClassControl
	} else {
		st.Class = maxClass
	}
	return st, parts, spans, nil
}

// opensStatementBody reports whether the words immediately before a '('
// introduce a statement body rather than a list or an expression. w1 is the
// nearest word.
//
// `AS (` is the CTE/subquery form. `AS MATERIALIZED (` and
// `AS NOT MATERIALIZED (` are PostgreSQL 12+ planning hints on the same
// construct — missing them meant `WITH x AS MATERIALIZED (DELETE FROM t)`
// recorded no nested mutation at all, so the WHERE guard had nothing to
// refuse and a full-table delete ran.
func opensStatementBody(w1, w2, w3 string) bool {
	switch {
	case w1 == "AS":
		return true
	case w1 == "MATERIALIZED" && w2 == "AS":
		return true
	case w1 == "MATERIALIZED" && w2 == "NOT" && w3 == "AS":
		return true
	}
	return false
}

// scanQuoted advances past a quoted region opened at s[i] and returns the
// index after the closing quote. Doubling always escapes; backslash escapes
// apply in MySQL mode to '-' and '"'-quoted strings (never backticks), and to
// a PostgreSQL E'…' string, whose caller passes true.
func scanQuoted(s string, i int, quote byte, backslashEscapes bool) (int, error) {
	j := i + 1
	n := len(s)
	for j < n {
		c := s[j]
		if backslashEscapes && quote != '`' && c == '\\' {
			j += 2
			continue
		}
		if c == quote {
			if j+1 < n && s[j+1] == quote {
				j += 2 // doubled quote
				continue
			}
			return j + 1, nil
		}
		j++
	}
	return 0, fmt.Errorf("%w: unterminated %q-quoted region", ErrMalformedStatement, string(quote))
}

// scanDollarQuote handles Postgres $tag$…$tag$ regions. Returns ok=false
// (without consuming) when s[i:] is not a dollar-quote opener — e.g. a
// positional parameter like $1.
func scanDollarQuote(s string, i int) (int, bool, error) {
	n := len(s)
	j := i + 1
	for j < n && (isASCIILetter(s[j]) || s[j] == '_' || (j > i+1 && s[j] >= '0' && s[j] <= '9')) {
		j++
	}
	if j >= n || s[j] != '$' {
		return 0, false, nil
	}
	delim := s[i : j+1]
	end := strings.Index(s[j+1:], delim)
	if end < 0 {
		return 0, false, fmt.Errorf("%w: unterminated dollar-quoted region %s", ErrMalformedStatement, delim)
	}
	return j + 1 + end + len(delim), true, nil
}

// scanTclParameter advances past a SQLite named parameter opened at s[i] by
// $, @, : or #, read as SQLite's tokenizer reads one: name characters and ::
// separators, then — after at least one name character — an optional (…)
// suffix ending at the first ), which may not contain whitespace. A prefix
// with no name, and a suffix cut off by whitespace or the end of the text,
// are tokens SQLite refuses; so does this.
func scanTclParameter(s string, i int) (int, error) {
	n := len(s)
	named := false
	j := i + 1
	for j < n {
		c := s[j]
		if isWordChar(c) {
			named = true
			j++
			continue
		}
		if c == ':' && j+1 < n && s[j+1] == ':' {
			j += 2
			continue
		}
		if c == '(' && named {
			k := j + 1
			for k < n && !isSpace(s[k]) && s[k] != ')' {
				k++
			}
			if k >= n || s[k] != ')' {
				return 0, fmt.Errorf("%w: unterminated parameter name", ErrMalformedStatement)
			}
			return k + 1, nil
		}
		break
	}
	if !named {
		return 0, fmt.Errorf("%w: parameter %q has no name", ErrMalformedStatement, s[i:i+1])
	}
	return j, nil
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func isWordStart(c byte) bool { return isASCIILetter(c) || c == '_' || c >= 0x80 }

func isWordChar(c byte) bool {
	return isASCIILetter(c) || c == '_' || (c >= '0' && c <= '9') || c == '$' || c >= 0x80
}
