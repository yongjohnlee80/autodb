package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
)

// SocketName is the default rendezvous filename for the local endpoint.
const SocketName = "autodb.sock"

// Endpoint is where the server listens and every frontend dials. It is
// resolved in ONE place because it is a rendezvous: the TUI and each
// Neovim instance share a daemon only by looking in the same spot, and
// two components computing "the same" address independently is how they
// end up in different places (ADR-0058 §3.2.1).
type Endpoint struct {
	// Network is "unix" or "tcp", ready for net.Listen and net.Dial.
	Network string
	// Address is a socket PATH for unix, host:port for tcp.
	Address string
}

// IsLocal reports whether this endpoint is unreachable from another
// machine by construction rather than by policy.
func (e Endpoint) IsLocal() bool { return e.Network == "unix" }

// String renders the endpoint for humans and for sys.hello's addr.
func (e Endpoint) String() string { return e.Address }

// Endpoint resolves where to listen or dial.
//
// **A configured port opts into TCP.** With no port, the endpoint is a
// unix socket, which cannot be reached off this machine at all and is
// guarded by filesystem permissions rather than by an address allowlist
// — the same guarantee Neovim's own RPC socket relies on. TCP remains
// available for the remote case, but it has to be asked for: exposing a
// credential-holding service beyond one machine is a decision an
// operator makes explicitly, never one they inherit from a default.
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

// maxSocketPath is the shortest sun_path across the platforms autodb
// targets: 104 bytes on darwin, 108 on Linux. The limit is in the
// kernel struct, so a path that fits in a filesystem can still fail to
// bind — with a confusing "invalid argument" rather than anything
// mentioning length. Checked here so the error names the real problem.
const maxSocketPath = 100

// runtimeDir picks the directory for the socket. Portable across the
// platforms autovim runs on, which is why it is not simply
// XDG_RUNTIME_DIR:
//
//   - Linux: XDG_RUNTIME_DIR (/run/user/$UID) — already 0700 and owned
//     by the user, and on tmpfs, so a socket cannot survive a reboot to
//     confuse the next launch.
//   - macOS: no XDG_RUNTIME_DIR exists. os.TempDir() resolves $TMPDIR,
//     which on darwin is a PER-USER directory under /var/folders at
//     mode 0700 — the same property we want, arrived at differently.
//   - Anything else: $XDG_STATE_HOME/autodb, created 0700.
//
// Unix domain sockets themselves are native on both (they predate Linux
// on the BSD side, and Neovim's own --listen uses them on macOS too), so
// only the LOCATION is platform-specific.
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
