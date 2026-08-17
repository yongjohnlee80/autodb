package rpc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// LaunchNonce is the one-time proof that this process is the child a
// particular launcher started (ADR-0058 §3.2.1).
//
// The problem it solves is narrow and worth stating exactly. sys.hello
// is unauthenticated, and everything it reports — pid, instance,
// address — is SELF-ASSERTED. A frontend that spawns a daemon and then
// trusts the next hello it receives has proved nothing: a process that
// already owns the port can answer plausibly, and the credentials the
// frontend sends next are a bootstrap or login passphrase. Looking up a
// start time and executable for the pid the peer claims only proves
// that some unrelated process exists.
//
// The nonce closes that gap without any OS socket attribution. The
// launcher writes a high-entropy value to a 0600 file before spawning
// and passes the PATH on the command line — the path is public, the
// contents are not — and the child returns the value in its hello. Only
// the process that was actually launched with that file can do so, and
// the proof arrives over the very connection being authenticated.
//
// What it does NOT do, stated so nobody relies on more: it is a LAUNCH
// proof, not a bearer secret. Once the daemon is running it will return
// the nonce to any bare hello caller on the loopback port. That is
// tolerable under the threat model of ADR-0058 §3.2.1 — the boundary is
// this host, and a process running as the same user has easier routes
// to the user's credentials — because a fresh nonce is scoped to a
// fresh spawn, and by the time it can be read the daemon already owns
// the port an impersonator would have needed to take first.
type LaunchNonce struct {
	value string
}

// ReadLaunchNonce consumes the nonce file at path: it reads the value
// and UNLINKS the file immediately, so the secret exists on disk for as
// little time as possible.
//
// A missing or unreadable path is an error, and callers must treat it
// as fatal rather than serving without a nonce. A launch that silently
// degrades to unprovable is the failure this whole mechanism exists to
// prevent (ADR-0058 §3.2.1: "fail to start rather than serve without").
func ReadLaunchNonce(path string) (*LaunchNonce, error) {
	raw, err := os.ReadFile(path)
	// Unlink regardless of the read outcome: a file we could not use is
	// still a secret we should not leave lying around.
	if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
		return nil, fmt.Errorf("rpc: removing launch nonce %s: %w", path, rmErr)
	}
	if err != nil {
		return nil, fmt.Errorf("rpc: reading launch nonce: %w", err)
	}
	v := strings.TrimSpace(string(raw))
	if v == "" {
		return nil, fmt.Errorf("rpc: launch nonce file %s is empty", path)
	}
	return &LaunchNonce{value: v}, nil
}

// Digest returns the SHA-256 of the nonce, hex-encoded.
//
// The launcher persists this digest in its spawn record rather than the
// nonce itself: the record must survive long enough to verify a child
// after the nonce file is gone, and a verifier that is not also a copy
// of the secret is the weaker thing to leave on disk.
func (n *LaunchNonce) Digest() string {
	if n == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(n.value))
	return hex.EncodeToString(sum[:])
}

// String deliberately does NOT reveal the value. The nonce is a
// credential, and the surest way to keep it out of logs and traces is
// to make the obvious accident impossible.
func (n *LaunchNonce) String() string {
	if n == nil {
		return "<no launch nonce>"
	}
	return "<launch nonce redacted>"
}

// WithLaunchNonce supplies the nonce this process was started with. The
// server echoes it in sys.hello so the launcher can recognise its own
// child; without this option, hello simply omits the field.
func WithLaunchNonce(n *LaunchNonce) Option {
	return func(o *options) { o.launchNonce = n }
}
