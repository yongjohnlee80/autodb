package main

import (
	"net"
	"testing"

	"github.com/yongjohnlee80/golib/logger"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/pressure"
	"github.com/yongjohnlee80/autodb/rpc"
)

// THE CONNECTION THIS PINS, and why it needs its own cell.
//
// core/config proves Endpoint.HostLocalOnly() classifies an address. rpc proves
// (*Server).wireErr discloses a cause only when discloseDetail is set. Both are
// green with the WIRE BETWEEN THEM CUT: replace the production
// WithDetailDisclosure(ep.HostLocalOnly()) with a constant, or delete it, and
// every one of those cells still passes while the shipped host-local TUI goes
// back to rendering "internal error" — or an off-host bind quietly starts
// publishing the target's identity.
//
// Measured, before this cell existed: substituting the constant false in
// runServe left ./rpc, ./core/config and ./cmd/autodb all green.
//
// So this asserts the assembled SERVER's capability, reached through the same
// rpcServerOptions the daemon uses, for each endpoint shape that matters.
func TestDisclosureWiring_TheBoundEndpointDecidesTheServerCapability(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ep   config.Endpoint
		want bool
	}{
		{"unix socket is host-local", config.Endpoint{Network: "unix", Address: "/run/user/1000/autodb.sock"}, true},
		{"loopback tcp is host-local", config.Endpoint{Network: "tcp", Address: "127.0.0.1:7419"}, true},
		{"ipv6 loopback is host-local", config.Endpoint{Network: "tcp", Address: "[::1]:7419"}, true},

		// The two that must NOT disclose. A wildcard bind is reached over
		// loopback constantly, which is exactly why classifying it by how it is
		// usually reached rather than by what it ACCEPTS would be wrong.
		{"wildcard tcp is not host-local", config.Endpoint{Network: "tcp", Address: "0.0.0.0:7419"}, false},
		{"routable tcp is not host-local", config.Endpoint{Network: "tcp", Address: "192.168.1.10:7419"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })

			// Built through the production option set, not by hand: a test that
			// re-spelled WithDetailDisclosure(ep.HostLocalOnly()) itself would
			// pass with the daemon's own call deleted.
			srv := rpc.New(nil, nil, config.Server{}, "wiring-cell",
				rpcServerOptions(tc.ep, ln, logger.Nop{}, t.TempDir(),
					func() rpc.FrontDoorInfo { return rpc.FrontDoorInfo{} },
					func() (pressure.Snapshot, error) { return pressure.Snapshot{}, nil },
				)...)

			if got := srv.DisclosesDetail(); got != tc.want {
				t.Errorf("endpoint %s %q assembled a server with DisclosesDetail()=%v, want %v",
					tc.ep.Network, tc.ep.Address, got, tc.want)
			}
		})
	}
}

// The option set must actually CARRY the disclosure decision. If the option is
// dropped from rpcServerOptions the cell above still fails for the host-local
// rows (the field defaults to false), but this states the obligation directly
// so the reason the option exists is not left implicit in a default.
func TestDisclosureWiring_TheOptionSetIsNotSilentAboutDisclosure(t *testing.T) {
	t.Parallel()

	local := config.Endpoint{Network: "unix", Address: "/run/user/1000/autodb.sock"}
	offHost := config.Endpoint{Network: "tcp", Address: "0.0.0.0:7419"}

	build := func(ep config.Endpoint) *rpc.Server {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		return rpc.New(nil, nil, config.Server{}, "wiring-cell",
			rpcServerOptions(ep, ln, logger.Nop{}, t.TempDir(),
				func() rpc.FrontDoorInfo { return rpc.FrontDoorInfo{} },
				func() (pressure.Snapshot, error) { return pressure.Snapshot{}, nil },
			)...)
	}

	// Two endpoints that differ ONLY in locality must produce servers that
	// differ in this capability. A constant — of either value — collapses them.
	if build(local).DisclosesDetail() == build(offHost).DisclosesDetail() {
		t.Fatal("a host-local and an off-host endpoint assembled the same disclosure " +
			"capability: the production wiring is a constant, not ep.HostLocalOnly()")
	}
}
