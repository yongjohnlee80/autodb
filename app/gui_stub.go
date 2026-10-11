//go:build !gui

// gui_stub.go — --gui refused with the reason, when the binary was built
// without the `gui` tag: this build carries no windowed backend (golib/gui
// needs cgo and the platform's window libraries), and a flag that silently
// opened a terminal instead — or failed to link at all — would read as
// accepted. The release binaries and `make build-gui` carry the real one.
package app

import (
	"context"
	"errors"
)

// guiDispatch refuses: this binary was built without the gui tag.
func guiDispatch(ctx context.Context, configPath, remoteProfile string, b build) error {
	_ = ctx
	_ = configPath
	_ = remoteProfile
	_ = b
	return errNoGui
}

// errNoGui says what to run instead: --ui is the same program in a terminal,
// and the tagged build is how the windowed one is made.
var errNoGui = errors.New("--gui was not built into this binary: it needs cgo and the platform's " +
	"window libraries (build with the `gui` tag, or run --ui for the same program in a terminal)")
