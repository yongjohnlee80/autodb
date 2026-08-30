package exec

import (
	"errors"
	"strings"
	"testing"
)

// The corpus replay (TestCorpusReplay) needs a 2.3 MB estate this repo does
// not ship, so it skips in CI. These are the SHAPES it found, distilled into
// cases that run everywhere — the findings kept, the proprietary schema not.
// Each one is a real thing the corpus contained.

func TestCorpusShapes(t *testing.T) {
	t.Parallel()

	t.Run("a column named after a verb is an identifier, not a verb", func(t *testing.T) {
		t.Parallel()

		// 12 corpus files could not even be SPLIT because of this: a
		// `comment` column made the whole CREATE TABLE read as DDL nested
		// inside a statement. COMMENT is a DDL verb; `comment` here is a
		// column name, and the scanner could not tell the difference.
		for _, tc := range []struct {
			sql   string
			verb  string
			class Class
		}{
			{"CREATE TABLE IF NOT EXISTS comment (id BIGSERIAL PRIMARY KEY, comment TEXT)", "CREATE", ClassDDL},
			{"INSERT INTO notes (id, comment) VALUES (1, 'x')", "INSERT", ClassWrite},
			// The authorization consequence, which is the serious half: this
			// classified as ddl, so a reader holding a read grant was refused
			// an ordinary SELECT.
			{"SELECT comment FROM notes", "SELECT", ClassRead},
			// Position must not matter. Reading the first word of ANY paren
			// as a verb left the bug alive wherever the offending column came
			// first (lector r0 MF2).
			{"SELECT (comment)", "SELECT", ClassRead},
			{"SELECT (comment) FROM notes", "SELECT", ClassRead},
			{"INSERT INTO notes (comment, id) VALUES ('x', 1)", "INSERT", ClassWrite},
			{"CREATE TABLE x (comment text, id int)", "CREATE", ClassDDL},
			{"SELECT id, comment FROM notes WHERE id = 1", "SELECT", ClassRead},
			{"SELECT merge FROM t", "SELECT", ClassRead},
			{"UPDATE notes SET comment = 'x' WHERE id = 1", "UPDATE", ClassWrite},
		} {
			st, err := Classify(tc.sql, false)
			if err != nil {
				t.Errorf("Classify(%q) = %v, want a classification", tc.sql, err)
				continue
			}
			if st.Verb != tc.verb || st.Class != tc.class {
				t.Errorf("Classify(%q) = %s/%s, want %s/%s", tc.sql, st.Verb, st.Class, tc.verb, tc.class)
			}
		}
	})

	t.Run("a quoted alias does not make a column list a statement body", func(t *testing.T) {
		t.Parallel()

		// PostgreSQL's column-alias list. Live PG accepts
		// `SELECT * FROM (SELECT 1) AS "x" (comment);` and returns 1.
		//
		// The quoted alias used to be consumed without disturbing the run of
		// words behind it, so AS was still the nearest word when `(comment)`
		// opened and the alias list was read as a CTE body — a valid query
		// refused, and the unquoted spelling of the same query accepted.
		// Only whitespace and comments are transparent to AS adjacency now.
		for _, sql := range []string{
			`SELECT * FROM (SELECT 1) AS "x" (comment)`,
			`SELECT * FROM (SELECT 1) AS x (comment)`,
			`SELECT * FROM (SELECT 1) AS "my table" (comment, id)`,
			"SELECT * FROM (SELECT 1) AS `x` (comment)",
		} {
			st, err := Classify(sql, false)
			if err != nil {
				t.Errorf("Classify(%s) = %v, want a read", sql, err)
				continue
			}
			if st.Class != ClassRead {
				t.Errorf("Classify(%s) class = %s, want read", sql, st.Class)
			}
			if len(st.Nested) != 0 {
				t.Errorf("Classify(%s) recorded nested mutations %+v", sql, st.Nested)
			}
		}
	})

	t.Run("a real CTE body is still a statement body", func(t *testing.T) {
		t.Parallel()

		// The fix above must not blind the scanner to the thing it is for: a
		// mutation after `AS (` is a statement body and is still tracked.
		st, err := Classify("WITH x AS (DELETE FROM t WHERE id = 1 RETURNING id) SELECT * FROM x", false)
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Nested) != 1 || st.Nested[0].Verb != "DELETE" {
			t.Fatalf("Nested = %+v, want the DELETE recorded", st.Nested)
		}
		if st.Class != ClassWrite {
			t.Errorf("class = %s, want write — a read whose CTE writes is a write", st.Class)
		}
	})

	t.Run("the dominant script shape splits", func(t *testing.T) {
		t.Parallel()

		// 378 of the corpus's statements are BEGIN and 370 are COMMIT: the
		// estate is overwhelmingly `BEGIN; …; COMMIT;`, and the old lexer
		// aborted on the first one.
		parts, err := SplitStatements("BEGIN;\nALTER TABLE t ADD COLUMN c int;\nCOMMIT;", false)
		if err != nil {
			t.Fatalf("split: %v", err)
		}
		if len(parts) != 3 {
			t.Fatalf("split into %d parts, want 3: %q", len(parts), parts)
		}
	})

	t.Run("statements the old cap refused now run", func(t *testing.T) {
		t.Parallel()

		// The corpus's largest statement is 11,481 bytes — a view definition,
		// refused outright by the old 8 KiB cap (design doc G4).
		big := "CREATE VIEW v AS SELECT '" + strings.Repeat("x", 11_000) + "' AS c"
		if len(big) <= 8*1024 {
			t.Fatal("the fixture is no longer larger than the old cap")
		}
		if len(big) > DefaultMaxStatementBytes {
			t.Fatalf("fixture %d bytes exceeds the new cap %d", len(big), DefaultMaxStatementBytes)
		}
		st, err := Classify(big, false)
		if err != nil {
			t.Fatalf("Classify: %v", err)
		}
		if st.Class != ClassDDL {
			t.Errorf("class = %s, want ddl", st.Class)
		}
	})

	t.Run("verbs the corpus contains that the survey reported absent", func(t *testing.T) {
		t.Parallel()

		// The design doc's §7 sampling reported zero DO blocks. The full
		// replay finds 22, so the CALL/DO capability of ADR-0074 §6a is not
		// hypothetical for this workload — it is deferred over real usage.
		st, err := Classify("DO $$ BEGIN PERFORM 1; END $$", false)
		if err != nil {
			t.Fatalf("Classify: %v", err)
		}
		if st.Verb != "DO" || st.Class != ClassControl {
			t.Errorf("DO block = %s/%s, want DO/control", st.Verb, st.Class)
		}
		// And one LOCK, and one SET LOCAL — the G1/G2 gaps, both real.
		for _, sql := range []string{"LOCK TABLE t IN EXCLUSIVE MODE", "SET LOCAL lock_timeout = '5s'"} {
			st, err := Classify(sql, false)
			if err != nil {
				t.Errorf("Classify(%q) = %v", sql, err)
				continue
			}
			if st.Class != ClassControl {
				t.Errorf("Classify(%q) class = %s, want control", sql, st.Class)
			}
		}
	})

	t.Run("deliberately empty migrations report themselves empty", func(t *testing.T) {
		t.Parallel()

		// 10 corpus files are placeholders — "this file is blank because we
		// need to preserve our numbering", "no rollback here as this was a
		// bug", and four that are literally zero bytes.
		for _, sql := range []string{"", "-- no rollback here as this was a bug", "\n\n-- blank\n"} {
			if _, err := SplitStatements(sql, false); !errors.Is(err, ErrEmptyStatement) {
				t.Errorf("SplitStatements(%q) = %v, want ErrEmptyStatement", sql, err)
			}
		}
	})
}
