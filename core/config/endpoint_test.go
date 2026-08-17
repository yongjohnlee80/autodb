package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// No port means the local socket. This is the default an operator gets
// by not deciding, so it is asserted rather than assumed.
func TestEndpointDefaultsToUnixSocket(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	ep, err := Server{Bind: "127.0.0.1"}.Endpoint()
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if ep.Network != "unix" {
		t.Fatalf("network = %q, want unix", ep.Network)
	}
	if filepath.Base(ep.Address) != SocketName {
		t.Fatalf("address = %q, want a path ending in %s", ep.Address, SocketName)
	}
	if !ep.IsLocal() {
		t.Fatal("a unix endpoint must report IsLocal")
	}
}

// Setting a port is the opt-in to TCP — the whole point of the switch.
func TestEndpointPortOptsIntoTCP(t *testing.T) {
	t.Parallel()
	ep, err := Server{Port: 7419, Bind: "127.0.0.1"}.Endpoint()
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if ep.Network != "tcp" || ep.Address != "127.0.0.1:7419" {
		t.Fatalf("endpoint = %+v, want tcp 127.0.0.1:7419", ep)
	}
	if ep.IsLocal() {
		t.Fatal("a TCP endpoint is not local-by-construction")
	}
}

// JoinHostPort, not Sprintf: an IPv6 bind needs brackets or the address
// is unparseable.
func TestEndpointBracketsIPv6(t *testing.T) {
	t.Parallel()
	ep, err := Server{Port: 7419, Bind: "::1"}.Endpoint()
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if ep.Address != "[::1]:7419" {
		t.Fatalf("address = %q, want [::1]:7419", ep.Address)
	}
}

func TestEndpointHonoursExplicitSocket(t *testing.T) {
	t.Parallel()
	ep, err := Server{Socket: "/tmp/custom-autodb.sock"}.Endpoint()
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if ep.Network != "unix" || ep.Address != "/tmp/custom-autodb.sock" {
		t.Fatalf("endpoint = %+v", ep)
	}
}

// The kernel's sun_path limit is shorter than a filesystem path limit,
// and exceeding it fails with "invalid argument" — an error that names
// nothing useful. Ours names the real problem.
func TestEndpointRejectsOverlongSocketPath(t *testing.T) {
	t.Parallel()
	long := "/tmp/" + strings.Repeat("x", 200) + ".sock"
	_, err := Server{Socket: long}.Endpoint()
	if err == nil {
		t.Fatal("want an error for an overlong socket path")
	}
	if !strings.Contains(err.Error(), "kernel limit") {
		t.Fatalf("error should explain the limit, got: %v", err)
	}
}
