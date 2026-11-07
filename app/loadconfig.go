package app

import (
	"fmt"
	"io"
	"os"

	"github.com/yongjohnlee80/autodb/core/config"
	coreexec "github.com/yongjohnlee80/autodb/core/exec"
)

// warnTo is where a loaded configuration's warnings go: stderr, so a
// command's own output — --print-endpoint's line, --check-config's verdict —
// stays what a script parses.
var warnTo io.Writer = os.Stderr

// loadConfig is config.Load for every command, printing what the load found
// worth saying: keys this release does not know, which no longer refuse the
// load (docs/ops/schema-scripts.md) and so must be SAID or they are silently ignored.
func loadConfig(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return cfg, err
	}
	// The one rule config can't check itself: it may not import the engine
	// that owns the number. As a configuration refusal, so check-config exits
	// 78 on it like any other.
	if cfg.FrontDoor.Enabled {
		if lerr := coreexec.ValidateSessionLimits(cfg.Exec.MaxSessionsGlobal); lerr != nil {
			return cfg, fmt.Errorf("%w: %w", config.ErrInvalid, lerr)
		}
	}
	for _, w := range cfg.Warnings() {
		fmt.Fprintf(warnTo, "autodb: warning: %s\n", w)
	}
	return cfg, nil
}
