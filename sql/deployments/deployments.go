// Package deployments is the meta store's schema as SQL scripts, one
// directory per engine, compiled into the binary (docs/ops/schema-scripts.md).
//
//	postgres/000001_update_initialize_tables.sql   the baseline: no revert
//	postgres/000002_update_<slug>.sql              a change …
//	postgres/000002_revert_<slug>.sql              … and its undo
//	sqlite/  (the same names)
//
// Every script exists for both engines under the same number and slug, so the
// two ledgers always name the same scripts; a script with no work on one
// engine is a comment saying why. Numbers are dense and never reused. A
// released script never changes: its digest is recorded when it is applied,
// and a test holds the list of released digests.
package deployments

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"

	"github.com/yongjohnlee80/golib/parse"
	gsql "github.com/yongjohnlee80/golib/parse/sql"

	"github.com/yongjohnlee80/autodb/core/engine"
)

//go:embed postgres/*.sql sqlite/*.sql
var files embed.FS

// Engines are the meta store's engines; each one's scripts are in the
// directory named for it.
var Engines = []engine.Name{engine.Postgres, engine.SQLite}

// Kind is what a script does.
type Kind string

const (
	Update Kind = "update"
	Revert Kind = "revert"
)

// Script is one file.
type Script struct {
	Number int
	Kind   Kind
	Slug   string
	// Name is the file name, which the ledger records: 000001_update_initialize_tables.sql.
	Name string
	// Body is the file as written; SHA256 its digest, hex.
	Body   []byte
	SHA256 string
	engine engine.Name
}

// Statement is one statement of a script, and where it starts in the file.
type Statement struct {
	Text string
	Line int
}

var nameRE = regexp.MustCompile(`^(\d{6})_(update|revert)_([a-z0-9_]+)\.sql$`)

// Scripts are an engine's scripts, updates and reverts, by number then kind
// (update first). A file not named NNNNNN_(update|revert)_<slug>.sql is an
// error: a script the runner could not place would silently never run.
func Scripts(eng engine.Name) ([]Script, error) {
	dir := eng.String()
	entries, err := fs.ReadDir(files, dir)
	if err != nil {
		return nil, fmt.Errorf("deployments: no scripts for engine %q: %w", dir, err)
	}
	var out []Script
	for _, e := range entries {
		m := nameRE.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("deployments: %s/%s is not named NNNNNN_(update|revert)_<slug>.sql", dir, e.Name())
		}
		n, _ := strconv.Atoi(m[1])
		body, err := fs.ReadFile(files, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		out = append(out, Script{Number: n, Kind: Kind(m[2]), Slug: m[3], Name: e.Name(),
			Body: body, SHA256: hex.EncodeToString(sum[:]), engine: eng})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Number != out[j].Number {
			return out[i].Number < out[j].Number
		}
		return out[i].Kind == Update && out[j].Kind == Revert
	})
	return out, nil
}

// Updates are an engine's update scripts, in the order they apply.
func Updates(eng engine.Name) ([]Script, error) {
	all, err := Scripts(eng)
	if err != nil {
		return nil, err
	}
	var out []Script
	for _, s := range all {
		if s.Kind == Update {
			out = append(out, s)
		}
	}
	return out, nil
}

// RevertOf is the revert for update script number n, if it has one; the
// baseline, 000001, never does.
func RevertOf(eng engine.Name, n int) (Script, bool, error) {
	all, err := Scripts(eng)
	if err != nil {
		return Script{}, false, err
	}
	for _, s := range all {
		if s.Number == n && s.Kind == Revert {
			return s, true, nil
		}
	}
	return Script{}, false, nil
}

// Statements splits the script into statements with its engine's lexical
// rules — a semicolon inside a string, a comment or a PostgreSQL dollar-quoted
// body is not a boundary — each with the line it starts on, for an error that
// names where in the file it failed.
func (s Script) Statements() ([]Statement, error) {
	splitter := gsql.SQL{
		DollarQuotes:        s.engine.DollarQuotedStrings(),
		NestedBlockComments: s.engine.NestedBlockComments(),
		EStringEscapes:      s.engine.EscapeStringConstants(),
	}
	stmts, err := splitter.Parse(s.Body)
	if err != nil {
		return nil, fmt.Errorf("deployments: %s/%s: %w", s.engine, s.Name, err)
	}
	out := make([]Statement, 0, len(stmts))
	for _, st := range stmts {
		if onlyComments(st.Text) {
			continue // a script with no work on this engine says why, and runs nothing
		}
		out = append(out, Statement{Text: st.Text, Line: st.Pos.Line})
	}
	return out, nil
}

// onlyComments reports whether text is nothing but -- comments and space.
func onlyComments(text string) bool {
	sc := parse.NewScanner([]byte(text))
	for !sc.Done() {
		r, _ := sc.Next()
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
		case r == '-' && sc.Take("-"):
			for !sc.Done() {
				if r, _ := sc.Next(); r == '\n' {
					break
				}
			}
		default:
			return false
		}
	}
	return true
}
