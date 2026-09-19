package meta

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/yongjohnlee80/autodb/core/engine"
)

// StoreConfig defines the structural interface required by core/meta to open and configure
// the underlying database connection pool.
//
// Consumer-Driven Interface Decoupling:
// To maintain clean package boundaries and avoid circular import dependencies between core/config
// and core/meta, core/meta declares this consumer-side interface rather than importing config.Meta:
//
//	┌──────────────────────┐             ┌──────────────────────┐
//	│     core/config      │             │      core/meta       │
//	│                      │             │                      │
//	│ struct Meta {        │             │ type StoreConfig     │
//	│   StoreEngine() ...  │             │   interface { ... }  │
//	│   StorePath() ...    │             └──────────▲───────────┘
//	│   StoreDSN() ...     │                        │
//	│   StorePoolMax...    │                        │ Satisfies
//	│ }                    │────────────────────────┘ Structurally
//	└──────────────────────┘
//	(Zero compile-time import dependencies between packages)
//
// Any struct implementing these four accessors is accepted by Open and OpenNoMigrate.
type StoreConfig interface {
	// StoreEngine is the backend: postgres or sqlite.
	StoreEngine() engine.Name
	// StorePath is the sqlite file. Empty takes DefaultPath.
	StorePath() string
	// StoreDSN is the postgres connection string.
	StoreDSN() string
	// StorePoolMaxConns is the bound the meta pool will ACTUALLY use, already
	// resolved. This package takes the resolved number rather than the
	// configured one because "what did we configure" and "what will we use"
	// are different questions, and only the caller can answer the first.
	StorePoolMaxConns() int
}

// DefaultPath is the default sqlite meta-store location:
// $XDG_DATA_HOME/autodb/meta.db, falling back to ~/.local/share.
//
// It lives HERE rather than in core/config because it is a fact about where
// this store keeps its file, not a fact about how a config file is read — and
// because both of this package's users of it were reaching across the edge
// this change removes.
func DefaultPath() (string, error) {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("meta: resolving home dir: %w", err)
		}
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "autodb", "meta.db"), nil
}
