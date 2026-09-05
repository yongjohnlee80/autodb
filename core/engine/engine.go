// Package engine names the database engines autodb can target, and the ones it
// can keep its own metadata in.
//
// It owns that concept and nothing else. Every other package may depend on it,
// and it depends on nothing, so naming an engine can never pull a dependency
// cycle behind it.
//
// WHY A PACKAGE FOR THREE STRINGS. Before this existed the names were written
// as string literals at 50 sites in 22 files, and every engine DIFFERENCE was
// asked as an identity comparison against one of them (`connRow.Engine ==
// "mysql"`). Two consequences, and the second is the one that costs: a typo is
// a silent no-match rather than a compile error, and a fourth engine takes the
// default branch at every one of those sites without failing to build. The
// constants close the first. Capability interfaces (ADR-0088 §2) close the
// second; this package is what they are keyed on.
package engine

import (
	"fmt"
	"strings"
)

// Name is the canonical identity of a database engine.
//
// The underlying string is the spelling that is PERSISTED — in the meta store's
// connection rows and in the TOML config — so it is a stable format, not an
// internal label. Changing a constant's value is a migration, not a rename.
type Name string

// Postgres is the PostgreSQL engine. It is the only engine the front door can
// relay natively, and the only meta-store engine besides SQLite.
const Postgres Name = "postgres"

// MySQL is the MySQL engine. It is a target engine only: the front door speaks
// the PostgreSQL wire protocol and has no MySQL equivalent.
const MySQL Name = "mysql"

// SQLite is the SQLite engine. It is the default meta-store engine and is not
// offered as a target.
const SQLite Name = "sqlite"

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
