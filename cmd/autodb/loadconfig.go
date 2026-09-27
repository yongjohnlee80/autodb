package main

import (
	"fmt"
	"io"
	"os"

	"github.com/yongjohnlee80/autodb/core/config"
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
	for _, w := range cfg.Warnings() {
		fmt.Fprintf(warnTo, "autodb: warning: %s\n", w)
	}
	return cfg, nil
}
