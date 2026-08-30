package exec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/dao"
)

// ADR-0074 §8 — "the control-verb string never reaches the wire" is honest
// only if the options on it are actually read. These are the cells of the
// gate matrix that concern transaction options.

func TestParseTxControl_Accepted(t *testing.T) {
	t.Parallel()

	cases := []struct {
		sql    string
		action TxAction
		opts   dao.TxOptions
		chain  bool
	}{
		// The bare forms and their noise words.
		{sql: "BEGIN", action: TxBegin},
		{sql: "begin", action: TxBegin},
		{sql: "BEGIN WORK", action: TxBegin},
		{sql: "BEGIN TRANSACTION", action: TxBegin},
		{sql: "START TRANSACTION", action: TxBegin},
		// What SplitStatements actually hands this parser: the statement
		// keeps its terminator, and any comment trailing it before the next
		// statement comes along for the ride.
		{sql: "BEGIN;\n-- lock person so no new updates happen", action: TxBegin},
		{sql: "COMMIT;\n/* done */\n", action: TxCommit},
		{sql: "BEGIN;", action: TxBegin},
		{sql: "COMMIT", action: TxCommit},
		{sql: "COMMIT WORK", action: TxCommit},
		{sql: "END", action: TxCommit}, // END is COMMIT in PostgreSQL
		{sql: "ROLLBACK", action: TxRollback},
		{sql: "ROLLBACK TRANSACTION", action: TxRollback},

		// Access mode — the option the read-only guarantee rides on.
		{sql: "BEGIN READ ONLY", action: TxBegin, opts: dao.TxOptions{Access: dao.TxReadOnly}},
		{sql: "BEGIN READ WRITE", action: TxBegin, opts: dao.TxOptions{Access: dao.TxReadWrite}},
		{sql: "START TRANSACTION READ ONLY", action: TxBegin, opts: dao.TxOptions{Access: dao.TxReadOnly}},

		// The full isolation domain, including the two-word levels.
		{sql: "BEGIN ISOLATION LEVEL SERIALIZABLE", action: TxBegin, opts: dao.TxOptions{Isolation: dao.TxSerializable}},
		{sql: "BEGIN ISOLATION LEVEL REPEATABLE READ", action: TxBegin, opts: dao.TxOptions{Isolation: dao.TxRepeatableRead}},
		{sql: "BEGIN ISOLATION LEVEL READ COMMITTED", action: TxBegin, opts: dao.TxOptions{Isolation: dao.TxReadCommitted}},
		{sql: "BEGIN ISOLATION LEVEL READ UNCOMMITTED", action: TxBegin, opts: dao.TxOptions{Isolation: dao.TxReadUncommitted}},

		// Combined, in either order, with or without the commas PostgreSQL
		// allows. `READ COMMITTED READ ONLY` is the case a naive parser gets
		// wrong: the level's second word and the access mode's first word are
		// both READ.
		{
			sql:    "BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY",
			action: TxBegin,
			opts:   dao.TxOptions{Isolation: dao.TxSerializable, Access: dao.TxReadOnly},
		},
		{
			sql:    "BEGIN ISOLATION LEVEL READ COMMITTED READ ONLY",
			action: TxBegin,
			opts:   dao.TxOptions{Isolation: dao.TxReadCommitted, Access: dao.TxReadOnly},
		},
		{
			sql:    "BEGIN READ ONLY ISOLATION LEVEL SERIALIZABLE",
			action: TxBegin,
			opts:   dao.TxOptions{Isolation: dao.TxSerializable, Access: dao.TxReadOnly},
		},
		{
			sql:    "BEGIN ISOLATION LEVEL SERIALIZABLE, READ ONLY, DEFERRABLE",
			action: TxBegin,
			opts:   dao.TxOptions{Isolation: dao.TxSerializable, Access: dao.TxReadOnly, Deferrable: dao.TxDeferrable},
		},
		{
			sql:    "BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY NOT DEFERRABLE",
			action: TxBegin,
			opts:   dao.TxOptions{Isolation: dao.TxSerializable, Access: dao.TxReadOnly, Deferrable: dao.TxNotDeferrable},
		},

		// Chaining.
		{sql: "COMMIT AND CHAIN", action: TxCommit, chain: true},
		{sql: "ROLLBACK AND CHAIN", action: TxRollback, chain: true},
		{sql: "COMMIT AND NO CHAIN", action: TxCommit, chain: false},
		{sql: "ROLLBACK WORK AND NO CHAIN", action: TxRollback, chain: false},

		// Comments are not clauses.
		{sql: "BEGIN /* why not */ READ ONLY", action: TxBegin, opts: dao.TxOptions{Access: dao.TxReadOnly}},
		{sql: "BEGIN -- comment\n READ ONLY", action: TxBegin, opts: dao.TxOptions{Access: dao.TxReadOnly}},

		// Repeating an option with the SAME value is redundant, not
		// contradictory, and refusing it would be pedantry.
		{sql: "BEGIN READ ONLY READ ONLY", action: TxBegin, opts: dao.TxOptions{Access: dao.TxReadOnly}},

		// Commas SEPARATE modes and are optional. Verified accepted by
		// PostgreSQL 17.6.
		{
			sql:    "BEGIN ISOLATION LEVEL SERIALIZABLE, READ ONLY, DEFERRABLE",
			action: TxBegin,
			opts:   dao.TxOptions{Isolation: dao.TxSerializable, Access: dao.TxReadOnly, Deferrable: dao.TxDeferrable},
		},
		{
			sql:    "START TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ WRITE",
			action: TxBegin,
			opts:   dao.TxOptions{Isolation: dao.TxRepeatableRead, Access: dao.TxReadWrite},
		},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()

			got, err := ParseTxControl(tc.sql)
			if err != nil {
				t.Fatalf("ParseTxControl(%q) = %v", tc.sql, err)
			}
			if got.Action != tc.action {
				t.Errorf("action = %v, want %v", got.Action, tc.action)
			}
			if got.Options != tc.opts {
				t.Errorf("options = %+v, want %+v", got.Options, tc.opts)
			}
			if got.Chain != tc.chain {
				t.Errorf("chain = %v, want %v", got.Chain, tc.chain)
			}
		})
	}
}

// Malformed input is *dao.ErrTxOptionInvalid — the caller can fix it by
// retyping, and that is the distinction the error identity carries.
func TestParseTxControl_MalformedInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		sql  string
	}{
		{"unknown option", "BEGIN FROBNICATE"},
		{"isolation without LEVEL", "BEGIN ISOLATION SERIALIZABLE"},
		{"isolation without a level name", "BEGIN ISOLATION LEVEL"},
		{"unknown isolation level", "BEGIN ISOLATION LEVEL BANANAS"},
		{"repeatable without read", "BEGIN ISOLATION LEVEL REPEATABLE"},
		{"read without committed", "BEGIN ISOLATION LEVEL READ SOMETIMES"},
		{"access without a mode", "BEGIN READ"},
		{"unknown access mode", "BEGIN READ SIDEWAYS"},
		{"conflicting access modes", "BEGIN READ ONLY READ WRITE"},
		{"conflicting deferrable", "BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY DEFERRABLE NOT DEFERRABLE"},
		{"isolation set twice", "BEGIN ISOLATION LEVEL SERIALIZABLE ISOLATION LEVEL READ COMMITTED"},
		{"NOT without deferrable", "BEGIN NOT TODAY"},
		{"trailing garbage on commit", "COMMIT SOON"},
		{"chain misspelled", "COMMIT AND CHIAN"},
		{"and without chain", "COMMIT AND"},
		{"quoted text has no place here", "BEGIN 'READ ONLY'"},
		{"parenthesized", "BEGIN (READ ONLY)"},

		// Comma placement. Every one of these was accepted before commas
		// became tokens, and every one is rejected by PostgreSQL 17.6 —
		// checked against a live server rather than read off the grammar.
		{"leading comma", "BEGIN , READ ONLY"},
		{"comma inside a mode", "BEGIN READ, ONLY"},
		{"comma inside ISOLATION LEVEL", "BEGIN ISOLATION, LEVEL SERIALIZABLE"},
		{"trailing comma", "BEGIN READ ONLY,"},
		{"doubled comma", "BEGIN READ ONLY,, DEFERRABLE"},
		{"comma on an ending statement", "COMMIT , AND CHAIN"},
		{"comma tail on an ending statement", "ROLLBACK AND CHAIN,"},

		// START requires TRANSACTION; PostgreSQL rejects both of these.
		// Stripping one optional noise word for every verb alike made them
		// parse as a bare begin.
		{"bare START", "START"},
		{"START WORK", "START WORK"},

		// A savepoint target must be COMPLETE before it can be refused as a
		// capability — otherwise a typo is reported as a missing feature.
		{"rollback to nothing", "ROLLBACK TO"},
		{"rollback to a name with a tail", "ROLLBACK TO sp1 extra"},
		{"rollback to savepoint with a tail", "ROLLBACK TO SAVEPOINT sp1 extra"},
		// A savepoint name is an IDENTIFIER, and punctuation is not one.
		// PostgreSQL 17.6 answers both of these with `syntax error at or
		// near ","`, so reporting them as a missing capability would send the
		// caller hunting for a feature when what they have is a typo.
		{"comma as a savepoint target", "ROLLBACK TO ,"},
		{"comma after the SAVEPOINT keyword", "ROLLBACK TO SAVEPOINT ,"},
		{"empty delimited identifier", `ROLLBACK TO ""`},
		// A delimited identifier is lexed now, but it still names nothing
		// outside a savepoint target.
		{"quoted identifier as a transaction mode", `BEGIN "read only"`},
		{"quoted identifier on an ending statement", `COMMIT "x"`},
		// String literals are still not tokens here at all.
		{"string literal", "ROLLBACK TO 'sp1'"},

		// The DEFERRABLE combination rule, owned by golib-dao-0017 and
		// enforced here by asking the DAO rather than restating it.
		{"deferrable alone", "BEGIN DEFERRABLE"},
		{"not deferrable alone", "BEGIN NOT DEFERRABLE"},
		{"deferrable without serializable", "BEGIN READ ONLY DEFERRABLE"},
		{"deferrable without read only", "BEGIN ISOLATION LEVEL SERIALIZABLE DEFERRABLE"},
		{"deferrable with read write", "BEGIN ISOLATION LEVEL SERIALIZABLE READ WRITE DEFERRABLE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseTxControl(tc.sql)
			var invalid *dao.ErrTxOptionInvalid
			if !errors.As(err, &invalid) {
				t.Fatalf("ParseTxControl(%q) = %v, want *dao.ErrTxOptionInvalid", tc.sql, err)
			}
			// Malformed input is not a capability miss, and must not be
			// handled as one (golib-dao-0017 §2.2a).
			if errors.Is(err, dao.ErrUnsupported) {
				t.Error("a malformed statement must not read as a capability miss")
			}
			if errors.Is(err, ErrTxControlUnsupported) {
				t.Error("a malformed statement must not read as an unsupported clause")
			}
		})
	}
}

// A well-formed clause the engine will not carry is refused BY NAME. The
// point is that it is never silently dropped: a caller who asked for a
// consistent snapshot and quietly did not get one is worse off than one who
// was told no.
func TestParseTxControl_UnsupportedClauses(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, sql, mentions string }{
		{"mysql consistent snapshot", "START TRANSACTION WITH CONSISTENT SNAPSHOT", "CONSISTENT SNAPSHOT"},
		{"savepoint target", "ROLLBACK TO SAVEPOINT sp1", "SAVEPOINT"},
		{"savepoint target without the keyword", "ROLLBACK TO sp1", "SAVEPOINT"},
		// `ROLLBACK TO SAVEPOINT` is WELL-FORMED: PostgreSQL reads the
		// keyword as the name. Verified on 17.6 — it answers `savepoint
		// "savepoint" does not exist`, and succeeds when a savepoint by that
		// name is open. So it is a capability refusal, not a malformed one.
		{"savepoint named savepoint", "ROLLBACK TO SAVEPOINT", "SAVEPOINT"},
		// Every one of these is a savepoint name PostgreSQL 17.6 accepts —
		// verified by opening a savepoint so spelled and rolling back to it.
		// They must reach the capability refusal, not be rejected as
		// malformed on the way: a delimited identifier is how you spell a
		// name with a space in it.
		{"delimited identifier target", `ROLLBACK TO "My Point"`, "SAVEPOINT"},
		{"delimited identifier after the keyword", `ROLLBACK TO SAVEPOINT "My Point"`, "SAVEPOINT"},
		{"backtick-delimited target", "ROLLBACK TO SAVEPOINT `My Point`", "SAVEPOINT"},
		{"doubled quote inside the name", `ROLLBACK TO "My ""Point"""`, "SAVEPOINT"},
		{"dollar sign in the name", "ROLLBACK TO sp$1", "SAVEPOINT"},
		{"non-ascii name", "ROLLBACK TO spné", "SAVEPOINT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseTxControl(tc.sql)
			if !errors.Is(err, ErrTxControlUnsupported) {
				t.Fatalf("ParseTxControl(%q) = %v, want ErrTxControlUnsupported", tc.sql, err)
			}
			if !strings.Contains(err.Error(), tc.mentions) {
				t.Errorf("refusal %q does not name %q — a refusal that does not say what it refused is a shrug", err, tc.mentions)
			}
		})
	}
}

// Ordering, pinned by golib-dao-0017: malformed input is reported BEFORE a
// clause the engine cannot honor, even when the unsupported clause appears
// first in the statement. Told only about the unsupported clause, a caller
// removes it and hits the malformed-option error on the next attempt — two
// round trips for one statement.
func TestParseTxControl_InvalidIsReportedBeforeUnsupported(t *testing.T) {
	t.Parallel()

	// DEFERRABLE without SERIALIZABLE READ ONLY is malformed; WITH CONSISTENT
	// SNAPSHOT is unsupported. The unsupported clause is positionally first.
	const sql = "START TRANSACTION WITH CONSISTENT SNAPSHOT, DEFERRABLE"

	_, err := ParseTxControl(sql)
	var invalid *dao.ErrTxOptionInvalid
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want the malformed-input error to win", err)
	}
	if errors.Is(err, ErrTxControlUnsupported) {
		t.Error("the unsupported-clause refusal preempted the malformed-input one")
	}
	// And with the malformed option removed, the unsupported clause is what
	// remains — proving the first assertion was about ORDER and not about the
	// unsupported clause having gone unnoticed.
	if _, err := ParseTxControl("START TRANSACTION WITH CONSISTENT SNAPSHOT"); !errors.Is(err, ErrTxControlUnsupported) {
		t.Errorf("err = %v, want ErrTxControlUnsupported once the malformed option is gone", err)
	}
}

// The parser is handed one statement. It is not a splitter and must not
// quietly parse the first of several.
func TestParseTxControl_Boundaries(t *testing.T) {
	t.Parallel()

	if _, err := ParseTxControl("BEGIN; SELECT 1"); !errors.Is(err, ErrMultiStatement) {
		t.Errorf("multi-statement err = %v, want ErrMultiStatement", err)
	}
	// A second statement made entirely of WORDS. The case above is also
	// caught by the guard on non-word tokens (the `1`), so on its own it did
	// not prove the word path checks the terminator at all — removing that
	// check left the suite green. This one only passes if it does.
	if _, err := ParseTxControl("BEGIN; COMMIT"); !errors.Is(err, ErrMultiStatement) {
		t.Errorf("word-only multi-statement err = %v, want ErrMultiStatement", err)
	}
	if _, err := ParseTxControl("COMMIT; ROLLBACK"); !errors.Is(err, ErrMultiStatement) {
		t.Errorf("word-only multi-statement err = %v, want ErrMultiStatement", err)
	}
	// Comments and whitespace after the terminator are NOT a second
	// statement — that distinction is the whole of MF1.
	for _, sql := range []string{"BEGIN;", "BEGIN; -- trailing", "BEGIN;\n/* trailing */\n", "BEGIN ;  \n\n"} {
		if _, err := ParseTxControl(sql); err != nil {
			t.Errorf("ParseTxControl(%q) = %v, want a clean parse", sql, err)
		}
	}
	if _, err := ParseTxControl("   \n -- nothing here\n"); !errors.Is(err, ErrEmptyStatement) {
		t.Errorf("empty err = %v, want ErrEmptyStatement", err)
	}
	if _, err := ParseTxControl("SELECT 1"); !errors.Is(err, ErrStatementUnsupported) {
		t.Errorf("non-control err = %v, want ErrStatementUnsupported", err)
	}
	if _, err := ParseTxControl("BEGIN /* unterminated"); !errors.Is(err, ErrMalformedStatement) {
		t.Errorf("unterminated comment err = %v, want ErrMalformedStatement", err)
	}
}

// The splitter and the parser have to agree about what one statement is.
// They did not: SplitStatements returns a statement WITH its terminator and
// any comment trailing it, and this parser accepted only whitespace after the
// `;`. 153 of the 756 transaction controls in the LM deployment corpus failed
// the round trip — the atomic-script workload the session engine exists for.
//
// The corpus replay covers this at scale where the corpus is available; these
// are the shapes it found, pinned so they run everywhere.
func TestParseTxControl_ConsumesWhatSplitStatementsProduces(t *testing.T) {
	t.Parallel()

	scripts := []string{
		"BEGIN;\nUPDATE t SET a = 1 WHERE id = 1;\nCOMMIT;",
		"-- header comment\nBEGIN;\n-- lock person first\nUPDATE t SET a = 1 WHERE id = 1;\nCOMMIT;\n",
		"BEGIN;\n\n/* a block comment between statements */\n\nSELECT 1;\nCOMMIT;\n-- trailing",
		"START TRANSACTION;\nSELECT 1;\nROLLBACK;",
	}
	for _, script := range scripts {
		parts, err := SplitStatements(script, false)
		if err != nil {
			t.Fatalf("split(%q): %v", script, err)
		}
		for i, p := range parts {
			st, cerr := Classify(p, false)
			if cerr != nil {
				t.Errorf("part %d of %q: %v", i+1, script, cerr)
				continue
			}
			if st.Class != ClassControl {
				continue
			}
			if _, perr := ParseTxControl(p); perr != nil {
				t.Errorf("the splitter produced %q and the parser refused it: %v", p, perr)
			}
		}
	}
}

// Everything the classifier calls ClassControl with a transaction verb must
// be parseable or refused by name — never panic, never silently succeed with
// options nobody asked for. This is the join between the two halves of R2.
func TestParseTxControl_CoversEveryClassifiedTransactionVerb(t *testing.T) {
	t.Parallel()

	for _, sql := range []string{
		"BEGIN", "START TRANSACTION", "COMMIT", "END", "ROLLBACK",
		"BEGIN READ ONLY", "COMMIT AND CHAIN", "ROLLBACK AND CHAIN",
	} {
		st, err := Classify(sql, false)
		if err != nil {
			t.Fatalf("Classify(%q): %v", sql, err)
		}
		if st.Class != ClassControl {
			t.Fatalf("Classify(%q) class = %s, want control", sql, st.Class)
		}
		if _, err := ParseTxControl(sql); err != nil {
			t.Errorf("ParseTxControl(%q) = %v — the classifier accepts it, so the parser must too", sql, err)
		}
	}

	// A control verb that is NOT transaction control is rejected by the
	// parser rather than mangled into one: SET and LOCK are the session
	// engine's business, not this parser's.
	for _, sql := range []string{"SET LOCAL lock_timeout = '5s'", "LOCK TABLE t IN EXCLUSIVE MODE", "PRAGMA foreign_keys = OFF"} {
		if _, err := ParseTxControl(sql); !errors.Is(err, ErrStatementUnsupported) {
			t.Errorf("ParseTxControl(%q) = %v, want ErrStatementUnsupported", sql, err)
		}
	}
}

// The same round trip at production scale (ADR-0074 §8, G6). Env-gated like
// the classifier's corpus replay, and for the same reason: the corpus is
// another product's schema and stays in its own repo.
//
// This is the assertion that would have caught MF1 the day it was written.
// 153 of the 756 transaction controls in the LM deployment corpus failed
// split→parse, and every unit test in this file passed while they did,
// because they all fed the parser statements written by hand rather than
// statements produced by the splitter.
func TestParseTxControl_CorpusRoundTrip(t *testing.T) {
	dir := os.Getenv("AUTODB_CORPUS_DIR")
	if dir == "" {
		t.Skip("AUTODB_CORPUS_DIR not set; skipping the production-corpus round trip")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("AUTODB_CORPUS_DIR=%s holds no .sql files — an empty corpus proves nothing", dir)
	}

	// Statements in the corpus that are genuinely malformed SQL, so the
	// parser is right to refuse them. Named individually, with the evidence,
	// because "some failures are expected" is how a round trip stops being an
	// assertion.
	//
	// 000062_revert_trigger_track_error.sql opens with a `BEGIN` that has no
	// terminator, so the splitter correctly hands over
	// `BEGIN\n\nDROP FUNCTION …;` as one statement. PostgreSQL 17.6 answers
	// that with `syntax error at or near "DROP"` — the same complaint this
	// parser makes. The script cannot ever have run as written.
	knownMalformed := map[string]string{
		"000062_revert_trigger_track_error.sql:1": "BEGIN with no terminator; PostgreSQL rejects it too",
	}
	hit := map[string]bool{}

	var controls, failures int
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parts, err := SplitStatements(string(body), false)
		if err != nil {
			continue // empty placeholders; the classifier replay accounts for them
		}
		for i, p := range parts {
			st, cerr := Classify(p, false)
			if cerr != nil || st.Class != ClassControl {
				continue
			}
			switch st.Verb {
			case "BEGIN", "START", "COMMIT", "END", "ROLLBACK":
			default:
				continue // SET, LOCK, DO — not this parser's statements
			}
			controls++
			if _, perr := ParseTxControl(p); perr != nil {
				key := fmt.Sprintf("%s:%d", filepath.Base(f), i+1)
				if why, known := knownMalformed[key]; known {
					hit[key] = true
					t.Logf("%s: refused as expected (%s)", key, why)
					continue
				}
				failures++
				t.Errorf("%s: the splitter produced this and the parser refused it: %v\n%s",
					key, perr, p)
			}
		}
	}
	// An exception that stopped being needed is worse than none: it hides the
	// next regression at that spot.
	for key, why := range knownMalformed {
		if !hit[key] {
			t.Errorf("%s no longer fails (%s) — delete the exception rather than leaving it to cover something else", key, why)
		}
	}
	if controls == 0 {
		t.Fatal("no transaction controls found in the corpus — this corpus was chosen because it is " +
			"overwhelmingly BEGIN…COMMIT, so finding none means the replay is not reading it")
	}
	t.Logf("round-tripped %d transaction controls from %d files, %d failures", controls, len(files), failures)
}
