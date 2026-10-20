//go:build gui

// gui.go — the --gui launch path, built only with the `gui` tag: golib/gui
// needs cgo and the platform's window libraries (Wayland/X11 headers on
// Linux), which a default dev build and a CGO_ENABLED=0 release both refuse
// to carry. The release workflow builds the shipped binaries WITH the tag, so
// a brew install gets a working --gui; a source build without it refuses the
// flag with a message that says what to do instead of failing to link.
package app

import (
	"context"

	guilib "github.com/yongjohnlee80/golib/gui"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

// guiDispatch is --gui's arm of the dispatch switch. gui.Main keeps the
// platform's main thread (macOS's AppKit among them) and runs the program
// inside it — golib/gui's one contract.
func guiDispatch(ctx context.Context, configPath, remoteProfile string, b build) error {
	guilib.Main(func() error {
		return runGUI(ctx, configPath, remoteProfile, b)
	})
	return nil
}

// runGUI starts the standalone TUI in a native window: the same program as
// --ui on golib/gui's Backend — a tui.Backend that is a Gio window
// (golib/gui). The window shows the terminal program's every
// cell drawn natively; the program itself is unchanged.
//
// Called INSIDE gui.Main, which owns the platform's main thread.
func runGUI(ctx context.Context, configPath, remoteProfile string, b build) error {
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
	return runUIOn(ctx, guiBackend(), tuiapp.GUIStyle(), ep.Network, cfg, start, configPath, addr, notesRoot, b)
}

// guiBackend is --gui's window. Its style is golib/gui's default, NativeStyle: buttons,
// dropdowns, text fields and checkboxes are drawn as bordered controls, the TUI's cells
// everywhere else.
func guiBackend() *guilib.Backend {
	return guilib.NewBackend(
		guilib.WithTitle("autodb"),
		guilib.WithSize(1020, 640),
	)
}
