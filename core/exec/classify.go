package exec

import (
	"errors"
	"fmt"
	"strings"
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
	return 0
}

var (
	// ErrEmptyStatement reports a blank (or comment-only) script.
	ErrEmptyStatement = errors.New("exec: empty statement")
	// ErrMultiStatement reports content after a top-level ';' — one
	// statement per execution (ADR-0055 §1).
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
	// (lector M4 r2 must-fix #2).
	ErrScriptTooLarge = errors.New("exec: statement exceeds the maximum size")
)

// Statement is the classifier's verdict.
type Statement struct {
	// Verb is the classified main verb (uppercase), e.g. "SELECT", "DELETE".
	Verb string
	// Class is the authorization class: the MAXIMUM class of any verb in the
	// statement, so a read whose CTE body writes is authorized as a write
	// (data-modifying CTEs are rejected outright — see Classify — but the
	// escalation is defense in depth).
	Class Class
	// HasTopLevelWhere reports a WHERE at paren depth 0 after the main verb
	// (only meaningful for UPDATE/DELETE — the Objective 18 guard input).
	HasTopLevelWhere bool
}

// readVerbs are lexically read-only statements. PRAGMA is deliberately
// absent: SQLite has writable PRAGMA forms (e.g. `PRAGMA foreign_keys=OFF`),
// so PRAGMA is rejected as unsupported and introspection goes through the
// dedicated dao catalog API (ADR-0055 rev 1; lector M4 must-fix #2). SHOW /
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
}

// unsupportedVerbs are rejected loudly: transaction control, session state,
// and PRAGMA have no safe meaning on pooled autocommit connections
// (ADR-0055 §1).
var unsupportedVerbs = map[string]bool{
	"BEGIN": true, "START": true, "COMMIT": true, "END": true, "ROLLBACK": true,
	"SAVEPOINT": true, "RELEASE": true, "SET": true, "USE": true,
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

// Classify tokenizes one SQL script and returns its statement verdict.
// backslashEscapes selects MySQL string semantics (backslash escapes inside
// quotes, '#' line comments, /*! executable comments); leave false for
// postgres/sqlite.
//
// The authorization class is the maximum verb class anywhere in the
// statement. A write/DDL verb at paren depth > 0 (a data-modifying CTE such
// as `WITH x AS (DELETE ...) SELECT ...`, which PostgreSQL executes) is
// rejected outright — its embedded mutation cannot be WHERE-guarded, so v1
// does not run it (lector M4 must-fix #1).
func Classify(sqlText string, backslashEscapes bool) (Statement, error) {
	var st Statement
	var (
		depth       int
		searching   = true // still looking for the main verb
		inExplain   bool
		inWith      bool
		ended       bool // a top-level ';' was consumed
		sawContent  bool
		execComment bool // inside a MySQL /*! ... */ executable comment
	)
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
			return ErrMultiStatement
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
			// being wrongly commented out (lector M4 r2 must-fix #1).
			if backslashEscapes && i+2 < n && !isSpace(sqlText[i+2]) {
				if err := content(); err != nil {
					return st, err
				}
				i++ // consume one '-' as an operator; keep scanning
				continue
			}
			for i < n && sqlText[i] != '\n' {
				i++
			}

		case c == '#' && backslashEscapes:
			for i < n && sqlText[i] != '\n' {
				i++
			}

		case c == '/' && i+1 < n && sqlText[i+1] == '*':
			// MySQL executable comment /*![digits] ... */: the server RUNS
			// the body, so its tokens are live, not commented out. Skip only
			// the opener + optional version digits; the matching */ is
			// handled as whitespace below (lector M4 must-fix #3).
			if backslashEscapes && i+2 < n && sqlText[i+2] == '!' {
				j := i + 3
				for j < n && sqlText[j] >= '0' && sqlText[j] <= '9' {
					j++
				}
				execComment = true
				i = j
				continue
			}
			// PostgreSQL nests block comments; MySQL does not. Model the
			// target so a MySQL `/* a /* b */` doesn't leave a live tail
			// (lector M4 r2 must-fix #1).
			nests := !backslashEscapes
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
				return st, fmt.Errorf("%w: unterminated block comment", ErrMalformedStatement)
			}
			i = j

		case c == '*' && i+1 < n && sqlText[i+1] == '/' && execComment:
			execComment = false
			i += 2

		case c == '\'' || c == '"' || c == '`':
			if err := content(); err != nil {
				return st, err
			}
			j, err := scanQuoted(sqlText, i, c, backslashEscapes)
			if err != nil {
				return st, err
			}
			i = j

		case c == '$' && !backslashEscapes:
			if err := content(); err != nil {
				return st, err
			}
			if j, ok, err := scanDollarQuote(sqlText, i); err != nil {
				return st, err
			} else if ok {
				i = j
			} else {
				i++ // '$1' positional parameter etc.
			}

		case c == '(':
			if err := content(); err != nil {
				return st, err
			}
			depth++
			i++

		case c == ')':
			if err := content(); err != nil {
				return st, err
			}
			if depth > 0 {
				depth--
			}
			i++

		case c == ';':
			if depth == 0 {
				ended = true
			}
			i++

		case isWordStart(c):
			if err := content(); err != nil {
				return st, err
			}
			j := i + 1
			for j < n && isWordChar(sqlText[j]) {
				j++
			}
			word := strings.ToUpper(sqlText[i:j])
			i = j

			// EXPLAIN option words (ANALYZE, VERBOSE, FORMAT …) are not
			// verbs here — ANALYZE is also DDL, and options may sit inside
			// parens: `EXPLAIN (ANALYZE, BUFFERS) SELECT`. Skip them before
			// any class escalation or depth check.
			if inExplain && searching && explainSkip[word] {
				continue
			}

			if cls, ok := verbClass(word); ok {
				// A data-modifying verb below top level is a data-modifying
				// CTE/subquery — reject (its mutation can't be guarded).
				if depth > 0 && (cls == ClassWrite || cls == ClassDDL) {
					return st, fmt.Errorf("%w: data-modifying subquery/CTE (%s at nesting depth %d)", ErrStatementUnsupported, word, depth)
				}
				escalate(cls)
			}

			if depth != 0 {
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
					if unsupportedVerbs[word] {
						return st, fmt.Errorf("%w: %s (transaction control, session state, and PRAGMA have no safe meaning on pooled connections)", ErrStatementUnsupported, word)
					}
					return st, fmt.Errorf("%w: %s", ErrStatementUnsupported, word)
				}
				continue
			}
			if word == "WHERE" {
				st.HasTopLevelWhere = true
			}

		default:
			if err := content(); err != nil {
				return st, err
			}
			i++
		}
	}

	if execComment {
		return st, fmt.Errorf("%w: unterminated executable comment", ErrMalformedStatement)
	}
	if !sawContent {
		return st, ErrEmptyStatement
	}
	if searching {
		return st, fmt.Errorf("%w: unable to classify the statement", ErrStatementUnsupported)
	}
	st.Class = maxClass
	return st, nil
}

// scanQuoted advances past a quoted region opened at s[i] and returns the
// index after the closing quote. Doubling always escapes; backslash escapes
// apply in MySQL mode to '-' and '"'-quoted strings (never backticks).
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

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func isWordStart(c byte) bool { return isASCIILetter(c) || c == '_' || c >= 0x80 }

func isWordChar(c byte) bool {
	return isASCIILetter(c) || c == '_' || (c >= '0' && c <= '9') || c == '$' || c >= 0x80
}
