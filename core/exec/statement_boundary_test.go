package exec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	gsql "github.com/yongjohnlee80/golib/parse/sql"

	"github.com/yongjohnlee80/autodb/core/engine"
)

// STATEMENT BOUNDARIES EQUAL AUTODB'S.
//
// An editor finds the statement under its cursor with golib's SQL splitter and
// sends that text here to run. If the splitter and this package's lexer
// disagree about where a statement begins, the cursor can select text the gate
// reads differently: a comment the editor shows as a comment, run as a
// statement. So for each engine the two must agree on every script about where
// each statement begins and on whether the script is lexically broken.
//
// Neither lexer's text or span is compared: golib's span ends before the ';',
// and this package's runs to the next content, comments included, its first
// span from byte 0. Both begin a statement that FOLLOWS another at its first
// content byte, so each reads the script behind a probe statement, which makes
// the script's first statement a following one; the probe is dropped and the
// offsets shifted back. Equal starts mean the same count, none included, and
// the same partition.

// boundaryProbe is a statement every engine reads alike.
const boundaryProbe = "SELECT 0;\n"

// golibStarts is golib's statement starts for script: the first content byte of
// each statement, read behind the probe. errClass is "lexical" when the
// splitter refuses the script (an unterminated string, quote or comment).
func golibStarts(sp gsql.SQL, script string) (starts []int, errClass string) {
	sts, err := sp.Parse([]byte(boundaryProbe + script))
	if err != nil {
		return nil, "lexical"
	}
	if len(sts) == 0 || sts[0].Pos.Offset != 0 {
		return nil, "the probe was not read as the first statement"
	}
	for _, st := range sts[1:] {
		starts = append(starts, st.Pos.Offset-len(boundaryProbe))
	}
	return starts, ""
}

// autodbStarts is this package's statement starts for script, read the same way.
func autodbStarts(script string, eng engine.Name) (starts []int, errClass string) {
	spans, err := splitBoundaries(boundaryProbe+script, eng)
	if err != nil {
		if errors.Is(err, ErrMalformedStatement) {
			return nil, "lexical"
		}
		return nil, "unexpected: " + err.Error()
	}
	if len(spans) == 0 || spans[0].start != 0 {
		return nil, "the probe was not read as the first statement"
	}
	for _, sp := range spans[1:] {
		starts = append(starts, sp.start-len(boundaryProbe))
	}
	return starts, ""
}

// boundaryCases are the scripts where a lexical rule decides a boundary.
var boundaryCases = []string{
	"",
	"   \n\t ",
	"-- a comment",
	"/* a comment */",
	"# a comment",
	"/*+ DROP TABLE t */",
	"# DROP TABLE t",
	"/*+ DROP TABLE t */ SELECT 1",
	"SELECT 1; SELECT 2",
	"SELECT 1; SELECT 2;",
	"SELECT 1;; SELECT 2",
	"SELECT 1 # ; DROP TABLE t;",
	"SELECT 1 --; DROP TABLE t;\nSELECT 2",
	"SELECT 1 -- ; DROP TABLE t;\nSELECT 2",
	"SELECT 'a\\'; DROP TABLE t; --'",
	`SELECT "a\"; DROP TABLE t; --"`,
	"SELECT `a;b`; SELECT 2",
	"/*! SELECT 1; SELECT 2 */",
	"/*!50000 SELECT 1 ; SELECT 2 */",
	"SELECT 1 /*+ ; DROP TABLE t; */;",
	"SELECT $$a;b$$; SELECT 2",
	"SELECT $tag$ ; DROP TABLE t; $tag$; SELECT 2",
	"SELECT 1 /* /* */ ; DROP TABLE t; -- */",
	"/* a /* b */ c */ SELECT 1; SELECT 2",
	"SELECT E'\\''; DROP TABLE t; --'",
	"SELECT e'\\\\'; SELECT 2",
	"SELECT 1 AS [a'b]; DROP TABLE t; --'",
	"SELECT [a;b] FROM t; SELECT 2",
	"SELECT $a(x'y); DROP TABLE t; --'",
	"SELECT $$x; SELECT 2 $$",
	"SELECT :name, @name FROM t; SELECT 2",
	"CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; END; SELECT 2",
	"SELECT 'unterminated",
	"SELECT 1 /* unterminated",
	`SELECT "unterminated`,
}

// boundaryScripts are the cases, then every .sql file under AUTODB_CORPUS_DIR
// when it is set: the classifier corpus.
func boundaryScripts(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for i, c := range boundaryCases {
		out[fmt.Sprintf("case %02d %q", i, c)] = c
	}
	dir := os.Getenv("AUTODB_CORPUS_DIR")
	if dir == "" {
		t.Log("AUTODB_CORPUS_DIR not set: the adversarial cases only, not the corpus")
		return out
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("AUTODB_CORPUS_DIR=%s holds no .sql files (%v): an empty corpus proves nothing", dir, err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out["corpus "+filepath.Base(f)] = string(b)
	}
	t.Logf("%d scripts: %d cases and %d from the corpus", len(out), len(boundaryCases), len(files))
	return out
}

func TestStatementBoundaries_GolibSplitsAsTheGateReads(t *testing.T) {
	scripts := boundaryScripts(t)
	names := slices.Sorted(func(yield func(string) bool) {
		for n := range scripts {
			if !yield(n) {
				return
			}
		}
	})
	for _, eng := range []engine.Name{engine.Postgres, engine.MySQL, engine.SQLite} {
		t.Run(string(eng), func(t *testing.T) {
			sp := eng.StatementSplitter()
			for _, name := range names {
				script := scripts[name]
				gs, ge := golibStarts(sp, script)
				as, ae := autodbStarts(script, eng)
				if ge != ae || !slices.Equal(gs, as) {
					t.Errorf("%s: golib starts %v (%s), the gate's %v (%s)", name, gs, orNone(ge), as, orNone(ae))
				}
			}
		})
	}
}

func orNone(errClass string) string {
	if errClass == "" {
		return "no error"
	}
	return errClass
}
