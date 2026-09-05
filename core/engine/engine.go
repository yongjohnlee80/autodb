// Package engine names the database engines autodb can target, and the ones it
// can keep its own metadata in.
//
// It owns the TYPE and nothing else: the values are golib's. Every other
// package may depend on it, and it depends only on the standard library and
// golib's dao, so naming an engine can never pull a cycle behind it.
//
// WHY A PACKAGE FOR THREE STRINGS. Before this existed the names were written
// as string literals at 50 sites in 22 files, and every engine DIFFERENCE was
// asked as an identity comparison against one of them (`connRow.Engine ==
// "mysql"`).
//
// BE PRECISE ABOUT WHAT THE TYPE BUYS, because it is less than it looks. A
// defined string type does NOT turn a typo into a compile error: Go converts an
// untyped constant on assignment, so `var n Name = "postgress"` compiles
// happily. What the type buys is that a value cannot be confused with an
// unrelated string, and that every comparison has one canonical operand to
// compare against. The typo only becomes a build failure once raw literals are
// mechanically excluded from the rest of the tree, which is a separate guard
// and a separate change.
//
// The second cost — that a fourth engine takes the default branch at every one
// of those comparison sites without failing to build — is not addressed here at
// all. It is addressed by asking each site's real question as a capability the
// engine either has or does not, instead of asking which engine it is. This
// package is what those capabilities are keyed on.
package engine

import (
	"database/sql/driver"
	"fmt"
	"strings"

	"github.com/yongjohnlee80/golib/dao"
)

// Name is the canonical identity of a database engine.
//
// The underlying string is the spelling that is PERSISTED — in the meta store's
// connection rows and in the TOML config — so it is a stable format, not an
// internal label. Changing a constant's value is a migration, not a rename.
type Name string

// THE VALUES COME FROM golib, AND THAT IS THE POINT.
//
// Each constant below is DEFINED AS the corresponding dao dialect name rather
// than spelled again here. golib is the single source of truth for the strings
// (Johno, 2026-09-06: "always define constants in the upstream for a one source
// of truth"); autodb adds a TYPE and nothing else. If a golib value ever
// changed, these would follow at compile time — there is no second spelling
// that could stay behind.
//
// The type is what golib deliberately does not have: dao.Dialect declares
// `Name() string`, so typing the constants upstream would break that signature.
// Down here the value is PERSISTED — it is written to connection rows and read
// back — and a named type with a validating Scan is what turns an unknown
// stored spelling into an error at the read instead of a value nothing matches.

// Postgres is the PostgreSQL engine. It is the only engine the front door can
// relay natively, and the only meta-store engine besides SQLite.
const Postgres Name = dao.DialectPostgres

// MySQL is the MySQL engine. It is a target engine only: the front door speaks
// the PostgreSQL wire protocol and has no MySQL equivalent.
const MySQL Name = dao.DialectMySQL

// SQLite is the SQLite engine. It is the default meta-store engine and is not
// offered as a target.
const SQLite Name = dao.DialectSQLite

// All returns every engine name, in a fixed order.
//
// The order is stable so that error text and generated documentation do not
// change between runs. Callers must not modify the returned slice; it is
// freshly allocated on each call so that they cannot affect each other if they
// do.
func All() []Name {
	return []Name{Postgres, MySQL, SQLite}
}

// Parse converts a persisted or configured string to a Name.
//
// It accepts EXACTLY the members of All() and nothing else: no case folding, no
// aliases, no surrounding whitespace. That strictness is deliberate. The value
// arrives from a config file or a stored row, both of which a person edits, and
// silently accepting "Postgres" or "postgresql" would mean two spellings of one
// engine can exist in the store — after which any comparison, including a
// correct one, is a coin flip. A rejected name is a startup error a person can
// read; an accepted alias is a class of bug nobody sees.
func Parse(s string) (Name, error) {
	for _, n := range All() {
		if s == string(n) {
			return n, nil
		}
	}
	accepted := make([]string, 0, len(All()))
	for _, n := range All() {
		accepted = append(accepted, string(n))
	}
	return "", fmt.Errorf("engine: unknown engine %q (accepted: %s)",
		s, strings.Join(accepted, ", "))
}

// String returns the engine's persisted spelling.
func (n Name) String() string { return string(n) }

// Value implements driver.Valuer so a Name can be written to the meta store.
//
// WHY THIS IS NOT OPTIONAL. database/sql does not accept a defined string type
// as a parameter — it accepts string — so without this the field types cleanly,
// compiles, and then fails at runtime the first time a row is written. That is
// exactly what happened when the persisted field was first typed: conn.list
// came back as a bare "internal error" over the wire, two layers away from the
// cause.
func (n Name) Value() (driver.Value, error) { return string(n), nil }

// Scan implements sql.Scanner so a Name can be read back from the meta store.
//
// It accepts what a driver actually hands over for a text column — string,
// []byte, or NULL — and it VALIDATES: a stored value that is not a known engine
// is an error here rather than a Name that no comparison will ever match. A row
// written by an older build with a spelling this build does not know is a fault
// worth surfacing at the read, where the row id is still in hand.
func (n *Name) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		return fmt.Errorf("engine: NULL is not an engine name")
	case string:
		parsed, err := Parse(v)
		if err != nil {
			return err
		}
		*n = parsed
		return nil
	case []byte:
		parsed, err := Parse(string(v))
		if err != nil {
			return err
		}
		*n = parsed
		return nil
	default:
		return fmt.Errorf("engine: cannot scan %T into an engine name", src)
	}
}
