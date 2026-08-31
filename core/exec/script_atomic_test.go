package exec

import (
	"strings"
	"testing"
)

// Which scripts take the transactional path. This is asked of the CLASSIFIER,
// not by looking for the word BEGIN, because the classifier is what will run
// these statements — two different readings of one script is how it gets
// admitted down one path and executed down another.
func TestScriptOpensATransaction(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		parts []string
		want  bool
	}{
		{"plain statements", []string{"SELECT 1", "INSERT INTO t VALUES (1)"}, false},
		{"BEGIN", []string{"BEGIN", "SELECT 1", "COMMIT"}, true},
		{"START TRANSACTION", []string{"START TRANSACTION", "SELECT 1", "COMMIT"}, true},
		{"a bare COMMIT still counts", []string{"COMMIT"}, true},
		{"ROLLBACK counts", []string{"ROLLBACK"}, true},
		// SAVEPOINT does NOT open a transaction, so it does not by itself
		// divert a script onto the session path. It requires one to already
		// be open, and a script that has one has a BEGIN for this to find.
		// (Savepoints themselves are unimplemented — R3 refuses them as
		// pending, not as permanently inadmissible.)
		{"SAVEPOINT alone opens nothing", []string{"SAVEPOINT s1"}, false},

		// The word alone decides nothing. An identifier that merely READS
		// like a boundary must not divert a script onto the session path,
		// where its statements would run under a transaction the author
		// never asked for.
		{"begin as an identifier", []string{"SELECT begin FROM t"}, false},
		{"begin as a column name", []string{"INSERT INTO t (begin, id) VALUES (1, 2)"}, false},
		{"commit in a string", []string{"SELECT 'COMMIT'"}, false},
		{"commit in a comment", []string{"SELECT 1 -- COMMIT"}, false},

		// Unclassifiable text is not a boundary. It fails further down where
		// the error can name the statement, rather than silently steering
		// the whole script.
		{"malformed", []string{"SELECT 'unterminated"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := scriptOpensATransaction(tc.parts, false); got != tc.want {
				t.Errorf("scriptOpensATransaction(%q) = %v, want %v", tc.parts, got, tc.want)
			}
		})
	}
}

// A savepoint is refused as NOT YET BUILT, not as permanently inadmissible.
// The two messages tell a caller opposite things — one says wait, the other
// says stop — and a savepoint on a pinned session is a thing that can be made
// safe, so the permanent message was the wrong one.
func TestProfileSession_SavepointsAreRefusedAsPendingNotAsImpossible(t *testing.T) {
	t.Parallel()

	for _, sql := range []string{"SAVEPOINT s1", "RELEASE SAVEPOINT s1"} {
		st, err := Classify(sql, false)
		if err != nil {
			t.Fatalf("Classify(%q): %v", sql, err)
		}
		err = ProfileSession.admit(st, true)
		if err == nil {
			t.Fatalf("admit(%q) succeeded; savepoints are not implemented", sql)
		}
		if !strings.Contains(err.Error(), "not built yet") {
			t.Errorf("admit(%q) = %q; want the not-yet-built refusal", sql, err)
		}
		if strings.Contains(err.Error(), "cannot be made safe") {
			t.Errorf("admit(%q) says a savepoint cannot be made safe on a pooled connection; "+
				"on a PINNED session it can, and that message tells the caller to stop "+
				"trying when they should wait", sql)
		}
	}
}
