// Command autodb is the autodb binary: an RPC server, a standalone TUI, and
// (via the bundled Lua integration) the backend of autovim's dbase section.
//
// M0 scaffold: only --version is functional. Modes land per the roadmap —
// --serve (M5), --ui (M6), combined default (M6).
package main

import (
	"flag"
	"fmt"
	"os"
)

// version and commit are stamped at build time via
// -ldflags "-X main.version=<tag> -X main.commit=<sha>".
var (
	version = "dev"
	commit  = "none"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	serve := flag.Bool("serve", false, "run the RPC server (not yet implemented - roadmap M5)")
	ui := flag.Bool("ui", false, "run the standalone TUI (not yet implemented - roadmap M6)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("autodb %s (%s)\n", version, commit)
		return
	}

	switch {
	case *serve:
		fmt.Fprintln(os.Stderr, "autodb: --serve is not implemented yet (roadmap M5)")
	case *ui:
		fmt.Fprintln(os.Stderr, "autodb: --ui is not implemented yet (roadmap M6)")
	default:
		flag.Usage()
	}
	os.Exit(1)
}
