package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
)

// SocketName is the default rendezvous filename for the local MessagePack-RPC endpoint.
const SocketName = "autodb.sock"

// Endpoint represents the resolved network transport and socket address where the
// autodb daemon listens and frontends (such as the TUI or editor plugins) dial.
//
// Centralized rendezvous architecture:
// Resolving the endpoint in a single canonical function ensures that standalone clients,
// Neovim instances, and background daemons always converge on the exact same location.
//
//	                 [Server.Endpoint()]
//	                          │
//	                 s.Port > 0 (Configured)?
//	                          │
//	            YES ──────────┴────────── NO
//	             │                         │
//	             ▼                         ▼
//	    ┌─────────────────┐       ┌─────────────────┐
//	    │  Network: "tcp" │       │ Network: "unix" │
//	    │  Address:       │       │ Path:           │
//	    │  host:port      │       │ runtimeDir()/   │
//	    │                 │       │ autodb.sock     │
//	    └─────────────────┘       └────────┬────────┘
//	                                       │
//	                                       ▼
//	                         [Kernel Length Validation]
//	                         len(path) <= maxSocketPath (100B)
//	                         (Guards sockaddr_un sun_path limit)
type Endpoint struct {
	// Network is "unix" or "tcp", ready for net.Listen and net.Dial.
	Network string
	// Address is a filesystem socket PATH for unix, or host:port for tcp.
	Address string
}

// IsLocal reports whether this endpoint is unreachable from another machine
// by construction (Unix domain socket) rather than by network filtering policy.
func (e Endpoint) IsLocal() bool { return e.Network == "unix" }

// String renders the endpoint address for human-facing logging and the sys.hello RPC response.
func (e Endpoint) String() string { return e.Address }

// Endpoint resolves where the autodb daemon will listen or where frontends will dial.
//
// Resolution semantics:
//   - Setting Server.Port > 0 opts explicitly into TCP networking.
//   - Leaving Server.Port == 0 defaults to a local Unix domain socket.
//
// Security rationale:
// A Unix domain socket is physically restricted to local users and protected by standard
// POSIX filesystem permissions (0700), matching the security posture of Neovim's RPC socket.
// TCP networking exposes credential-bearing endpoints across the network and must be
// deliberately configured by an operator, never inherited as an implicit default.
func (s Server) Endpoint() (Endpoint, error) {
	if s.Port > 0 {
		// JoinHostPort, not Sprintf: an IPv6 bind ("::1") needs brackets.
		return Endpoint{
			Network: "tcp",
			Address: net.JoinHostPort(s.Bind, fmt.Sprintf("%d", s.Port)),
		}, nil
	}
	path := s.Socket
	if path == "" {
		dir, err := runtimeDir()
		if err != nil {
			return Endpoint{}, err
		}
		path = filepath.Join(dir, SocketName)
	}
	if len(path) > maxSocketPath {
		return Endpoint{}, fmt.Errorf(
			"%w: socket path %q is %d bytes, over the %d-byte kernel limit — "+
				"set server.socket to something shorter",
			ErrInvalid, path, len(path), maxSocketPath)
	}
	return Endpoint{Network: "unix", Address: path}, nil
}

// maxSocketPath defines the conservative length bound for Unix domain socket paths.
//
// Kernel sockaddr_un sun_path limits vary across operating systems:
//   - Darwin (macOS): 104 bytes
//   - Linux:          108 bytes
//
// Bound rationale:
// The 100-byte threshold provides a safe margin across all target platforms.
// Because the restriction originates in the kernel's sockaddr_un struct definition,
// a path exceeding this bound fails at net.Listen / bind(2) time with a misleading
// EINVAL ("invalid argument") error rather than an explicit path length diagnostic.
// Validating path length in Go produces an actionable error message naming the exact
// excess byte count.
const maxSocketPath = 100

// runtimeDir determines the appropriate secure filesystem directory for the Unix socket.
//
// Cross-platform resolution hierarchy:
//
//	                 [OS Environment Check]
//	                           │
//	                           ├──────────────── Linux ($XDG_RUNTIME_DIR)
//	                           │                 • /run/user/$UID
//	                           │                 • Mode 0700, owned by user
//	                           │                 • tmpfs: cleared automatically on reboot
//	                           │
//	                           ├──────────────── Darwin (os.TempDir())
//	                           │                 • $TMPDIR -> /var/folders/...
//	                           │                 • Per-user isolated directory
//	                           │                 • Mode 0700
//	                           │
//	                           └──────────────── Generic Fallback ($XDG_STATE_HOME)
//	                                             • $XDG_STATE_HOME/autodb (or ~/.local/state/autodb)
//	                                             • Explicitly created with mode 0700
func runtimeDir() (string, error) {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d, nil
	}
	if runtime.GOOS == "darwin" {
		// $TMPDIR on darwin is per-user and 0700. It is also periodically
		// swept, which is harmless: the socket is recreated on launch and
		// a stale file is cleared by the listener.
		if d := os.TempDir(); d != "" {
			return d, nil
		}
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("%w: no runtime directory and no home directory for the socket", ErrInvalid)
		}
		base = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(base, "autodb")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("%w: creating %s: %v", ErrInvalid, dir, err)
	}
	return dir, nil
}
