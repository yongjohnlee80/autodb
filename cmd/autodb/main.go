// Command autodb is the community build of the autodb binary: app.Main with
// no registrations — the program is the app package's, and this file owns
// only what the build stamps: version, commit and build date, passed in as
// app.Options. An enterprise build does the same with its own stamps and its
// own registrations, without copying the program.
package main

import (
	"context"
	"os"

	"github.com/yongjohnlee80/autodb/app"
)

// version, commit and buildDate are stamped at build time via
// -ldflags "-X main.version=<tag> -X main.commit=<sha> -X main.buildDate=<date>".
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

// exit is os.Exit; a test sees main's exit code through it.
var exit = os.Exit

func main() {
	exit(app.Main(context.Background(), os.Args[1:], app.Options{
		Version: version, Commit: commit, BuildDate: buildDate,
	}))
}
