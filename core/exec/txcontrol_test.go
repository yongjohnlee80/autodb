package exec

import (
	"errors"
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
