//go:build gui

// gui.go — the --gui launch path, built only with the `gui` tag: golib/gui
// needs cgo and the platform's window libraries (Wayland/X11 headers on
// Linux), which a default dev build and a CGO_ENABLED=0 release both refuse
// to carry. The release workflow builds the shipped binaries WITH the tag, so
// a brew install gets a working --gui; a source build without it refuses the
// flag with a message that says what to do instead of failing to link.
package main

import (
	"flag"

	guilib "github.com/yongjohnlee80/golib/gui"
)

// guiLaunch is the --gui flag: the standalone TUI in a native window.
var guiLaunch = flag.Bool("gui", false, "run the standalone TUI in a native window (golib/gui)")

// guiDispatch is --gui's arm of the dispatch switch. gui.Main keeps the
// platform's main thread (macOS's AppKit among them) and runs the program
// inside it — golib/gui's one contract.
func guiDispatch(configPath, remoteProfile string) error {
	guilib.Main(func() error {
		return runGUI(configPath, remoteProfile)
	})
	return nil
}

// runGUI starts the standalone TUI in a native window: the same program as
// --ui on golib/gui's Backend — a tui.Backend that is a Gio window
// (golib/gui, ADR 1791330692). The window shows the terminal program's every
// cell drawn natively; the program itself is unchanged.
//
// Called INSIDE gui.Main, which owns the platform's main thread.
func runGUI(configPath, remoteProfile string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	start, err := uiStart(cfg, remoteProfile, "")
	if err != nil {
		return err
	}
	ep, err := cfg.Server.Endpoint()
	if err != nil {
		return err
	}
	addr := ep.Address

	notesRoot, err := cfg.NotesRoot()
	if err != nil {
		return err
	}
	backend := guilib.NewBackend(
		guilib.WithTitle("autodb"),
		guilib.WithSize(1020, 640),
	)
	return runUIOn(backend, ep.Network, cfg, start, configPath, addr, notesRoot)
}
