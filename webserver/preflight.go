// Package webserver is autodb's `--web-ui` middleware: a thin web-server that
// terminates authentication, owns per-user RPC sessions, caps concurrency, and
// serves the EXISTING TUI component tree to a browser through golib/tui/web
// (ADR-0061).
//
// It is a frontend. The `--serve` daemon does not change — not one RPC method —
// and every obligation here exists because one process now serves N users where
// the terminal and Lua frontends served exactly one.
package webserver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yongjohnlee80/autodb/rpc"
)

// PreflightTimeout bounds the startup probe. Three seconds, matching `--serve`'s
// own occupancy probe: the two are asking the same question of the same address
// and should not disagree about how long the answer may take.
const PreflightTimeout = 3 * time.Second

// ErrNoDaemon reports that nothing is listening on the configured endpoint.
//
// Distinct from [ErrForeignOccupant] because the remedies are opposite, and a
// message that flattened them would send an operator to start a daemon on an
// address that is already taken (ADR-0061 §2.2, lector r1 #6).
var ErrNoDaemon = errors.New("webserver: no autodb daemon is listening")

// ErrForeignOccupant reports that something answered but is not a compatible
// autodb server — a foreign process, a stale unix socket, or a protocol
// mismatch.
var ErrForeignOccupant = errors.New("webserver: the address is occupied by something else")

// Preflight refuses to start when there is no daemon to serve.
//
// `--web-ui` never starts a backend (requirement 5), so a missing daemon is a
// startup failure rather than something to fix by spawning. Note the asymmetry
// with `--serve`, which exits ZERO when it finds a daemon already running: for
// that command an existing daemon means success, and for this one a missing
// daemon means failure. Sharing an exit convention between the two would be
// wrong in one direction or the other.
//
// This is only half the guarantee. It covers startup; a later reconnect is
// covered by passing a nil spawn function to the session, which makes
// auto-starting structurally impossible rather than merely unreached. A reviewer
// should check for both — only the second is a guarantee (§2.2).
func Preflight(ctx context.Context, network, addr string) (version string, err error) {
	probeCtx, cancel := context.WithTimeout(ctx, PreflightTimeout)
	defer cancel()

	version, err = rpc.ProbeOn(probeCtx, network, addr)
	switch {
	case err == nil:
		return version, nil
	case errors.Is(err, rpc.ErrNotAutodb):
		// Something is there. Telling the operator to start a daemon would send
		// them at an address that cannot be bound.
		return "", fmt.Errorf("%w: %s (%v)\n"+
			"  --web-ui found a listener that is not a compatible autodb daemon.\n"+
			"  Inspect or remove a stale socket, or point the config at the right\n"+
			"  endpoint. Do NOT start a daemon here — the address is taken.",
			ErrForeignOccupant, addr, err)
	default:
		return "", fmt.Errorf("%w: %s (%v)\n"+
			"  --web-ui does not start the backend. Run `autodb --serve` first\n"+
			"  (or `autodb --ui`, which does start one).",
			ErrNoDaemon, addr, err)
	}
}
