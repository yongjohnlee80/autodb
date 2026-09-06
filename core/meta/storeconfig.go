package meta

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/yongjohnlee80/autodb/core/engine"
)

// What the meta store needs in order to open itself.
//
// AN INTERFACE DECLARED BY THE CONSUMER, which is the whole point: core/meta
// used to import core/config to name the struct its own functions took, and a
// storage layer that imports the configuration layer is upside down. The
// dependency was not doing any work — this package reads exactly four values
// out of that struct and nothing else — so it cost a package edge to say
// "engine, path, DSN, pool bound".
//
// Declared here and satisfied STRUCTURALLY, so config.Meta continues to be
// accepted at every call site without core/config importing this package
// either. Neither layer names the other. The alternative shapes were both
// worse: a conversion function would have to live somewhere, and putting the
// method on config.Meta would point the edge the other way — pulling the
// database drivers into everything that reads a config file.
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
