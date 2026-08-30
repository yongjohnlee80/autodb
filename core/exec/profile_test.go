package exec

import (
	"errors"
	"strings"
	"testing"
)

// ADR-0074 §2 — the policy that left the tokenizer had to land somewhere, and
// this is where. The classifier tests prove control statements are now
// CLASSIFIED; these prove they are still REFUSED, with the same error
// identity, under the profile every existing surface runs.

func TestProfileV1Compat_RefusesEveryControlVerb(t *testing.T) {
	t.Parallel()

	// The full ADR-0074 §2 control-verb list, each in a shape a user could
	// plausibly type. Every one of them was refused by the tokenizer before
	// this change; every one of them is refused by the profile after it.
	cases := []string{
		"BEGIN",
		"BEGIN READ ONLY",
		"START TRANSACTION ISOLATION LEVEL SERIALIZABLE",
		"COMMIT",
		"COMMIT AND CHAIN",
		"END",
		"ROLLBACK",
		"SAVEPOINT sp1",
		"RELEASE SAVEPOINT sp1",
		"SET search_path = evil",
		"SET LOCAL lock_timeout = '5s'",
		"USE other_db",
		"ATTACH DATABASE 'x' AS y",
		"DETACH DATABASE y",
		"LOCK TABLE t IN EXCLUSIVE MODE",
		"UNLOCK TABLES",
		"CALL do_thing(1)",
		"DO $$ BEGIN PERFORM 1; END $$",
		"PREPARE p AS SELECT 1",
		"EXECUTE p",
		"DEALLOCATE p",
		"DECLARE c CURSOR FOR SELECT 1",
		"FETCH 1 FROM c",
		"COPY t FROM '/etc/passwd'",
		"PRAGMA foreign_keys = OFF",
	}
	for _, sql := range cases {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()

			st, err := Classify(sql, false)
			if err != nil {
				t.Fatalf("the lexer must classify this, not refuse it: %v", err)
			}
			if st.Class != ClassControl {
				t.Fatalf("class = %q, want %q", st.Class, ClassControl)
			}
			err = ProfileV1Compat.admit(st)
			if !errors.Is(err, ErrStatementUnsupported) {
				t.Fatalf("admit = %v, want ErrStatementUnsupported", err)
			}
			// The message names the verb, so a refusal is actionable rather
			// than a shrug (ADR-0074 §8a).
			if !strings.Contains(err.Error(), st.Verb) {
				t.Errorf("refusal %q does not name the verb %q", err, st.Verb)
			}
		})
	}
}

// Everything that ran before still runs: the profile has no opinion about a
// non-control statement.
func TestProfileV1Compat_AdmitsEveryExecutableClass(t *testing.T) {
	t.Parallel()

	for _, sql := range []string{
		"SELECT 1",
		"INSERT INTO t VALUES (1)",
		"UPDATE t SET a = 1 WHERE id = 1",
		"DELETE FROM t WHERE id = 1",
		"CREATE TABLE t (id int)",
		"REFRESH MATERIALIZED VIEW mv",
	} {
		st, err := Classify(sql, false)
		if err != nil {
			t.Fatalf("Classify(%q): %v", sql, err)
		}
		if err := ProfileV1Compat.admit(st); err != nil {
			t.Errorf("admit(%q) = %v, want nil", sql, err)
		}
	}
}

// An unknown profile refuses everything rather than falling back to the
// permissive one. A misconfigured surface must fail closed — the alternative
// is a typo in a config file quietly granting more than the operator wrote.
func TestProfile_UnknownFailsClosed(t *testing.T) {
	t.Parallel()

	st, err := Classify("SELECT 1", false)
	if err != nil {
		t.Fatal(err)
	}
	bogus := Profile("sesion") // a plausible typo
	if bogus.valid() {
		t.Fatal("the typo must not read as a known profile")
	}
	if err := bogus.admit(st); !errors.Is(err, ErrStatementUnsupported) {
		t.Errorf("admit under an unknown profile = %v, want a refusal", err)
	}
}

// ADR-0074 §6 — the guard applies its one rule at every depth.
func TestGuardWhere_NestedMutations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		sql     string
		refused bool
	}{
		// Top level, unchanged.
		{"top-level delete guarded", "DELETE FROM t WHERE id = 1", false},
		{"top-level delete bare", "DELETE FROM t", true},
		{"top-level update bare", "UPDATE t SET a = 1", true},

		// The coverage gap this closes: v1 refused BOTH of these, because it
		// could not tell them apart.
		{"cte delete guarded", "WITH x AS (DELETE FROM t WHERE id = 1 RETURNING id) SELECT * FROM x", false},
		{"cte delete bare", "WITH x AS (DELETE FROM t RETURNING id) SELECT * FROM x", true},
		{"cte update guarded", "WITH x AS (UPDATE t SET a = 1 WHERE id = 1 RETURNING id) SELECT * FROM x", false},
		{"cte update bare", "WITH x AS (UPDATE t SET a = 1 RETURNING id) SELECT * FROM x", true},

		// A WHERE belonging to an inner subquery is not a guard on the
		// mutation that encloses it. This is the case a depth-blind
		// "does the statement contain WHERE?" check gets wrong, and it is
		// the whole reason the guard input is per-depth.
		{
			"inner where does not guard the outer delete",
			"WITH x AS (DELETE FROM t RETURNING (SELECT 1 FROM u WHERE u.id = 1)) SELECT * FROM x",
			true,
		},

		// Several mutations, one of them bare: the guard refuses on the
		// unguarded one rather than being satisfied by its neighbour.
		{
			"one guarded one bare",
			"WITH a AS (DELETE FROM t WHERE id = 1 RETURNING id), b AS (UPDATE u SET x = 1 RETURNING id) SELECT 1",
			true,
		},
		{
			"both guarded",
			"WITH a AS (DELETE FROM t WHERE id = 1 RETURNING id), b AS (UPDATE u SET x = 1 WHERE id = 2 RETURNING id) SELECT 1",
			false,
		},

		// INSERT is unguarded at every depth, exactly as it is at top level:
		// there is no full-table hazard to state a predicate against, and a
		// rule invented for the nested case would not match the outer one.
		{"cte insert", "WITH x AS (INSERT INTO t VALUES (1) RETURNING id) SELECT * FROM x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			st, err := Classify(tc.sql, false)
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			err = guardWhere(st)
			if tc.refused && !errors.Is(err, ErrNoWhere) {
				t.Fatalf("guardWhere = %v, want ErrNoWhere", err)
			}
			if !tc.refused && err != nil {
				t.Fatalf("guardWhere = %v, want nil", err)
			}
		})
	}
}

// The nested mutations are reported with their depth, which is what lets the
// guard ask its question of the right one.
func TestClassify_NestedMutationDepths(t *testing.T) {
	t.Parallel()

	st, err := Classify("WITH x AS (DELETE FROM t WHERE id = 1 RETURNING id) SELECT * FROM x", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Nested) != 1 {
		t.Fatalf("Nested = %+v, want exactly one mutation", st.Nested)
	}
	n := st.Nested[0]
	if n.Verb != "DELETE" || n.Depth != 1 || !n.HasWhere {
		t.Errorf("Nested[0] = %+v, want DELETE at depth 1, guarded", n)
	}
	// A top-level mutation is NOT a nested one, and does not appear here.
	st, err = Classify("DELETE FROM t WHERE id = 1", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Nested) != 0 {
		t.Errorf("Nested = %+v, want empty for a top-level mutation", st.Nested)
	}
	if !st.HasTopLevelWhere {
		t.Error("HasTopLevelWhere = false for a guarded top-level delete")
	}
}

// The splitter shares the scanner, so aborting mid-scan on a control verb
// broke splitting too: a script containing BEGIN could not be split into its
// statements at all — which is exactly the shape 368 of the 470 scripts in
// the LM deployment corpus have.
func TestSplitStatements_ScriptWithTransactionControl(t *testing.T) {
	t.Parallel()

	const script = "BEGIN;\nUPDATE t SET a = 1 WHERE id = 1;\nCOMMIT;"
	parts, err := SplitStatements(script, false)
	if err != nil {
		t.Fatalf("a script containing BEGIN must split: %v", err)
	}
	want := []string{"BEGIN;", "UPDATE t SET a = 1 WHERE id = 1;", "COMMIT;"}
	if len(parts) != len(want) {
		t.Fatalf("split into %d parts (%q), want %d", len(parts), parts, len(want))
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Errorf("part %d = %q, want %q", i, parts[i], want[i])
		}
	}
	// And each part classifies on its own — the control verbs included.
	for _, p := range parts {
		if _, err := Classify(p, false); err != nil {
			t.Errorf("Classify(%q): %v", p, err)
		}
	}
}
