package config

import "testing"

// HostLocalOnly decides whether the RPC surface may disclose operator-facing
// error detail, so a wrong answer either leaks a target's identity off-host or
// withholds the diagnosis from the operator who owns the box. Both directions
// are asserted, including the addresses that LOOK local and are not.
func TestEndpoint_HostLocalOnly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ep   Endpoint
		want bool
	}{
		{"unix socket is local by construction", Endpoint{Network: "unix", Address: "/run/user/1000/autodb.sock"}, true},
		{"ipv4 loopback", Endpoint{Network: "tcp", Address: "127.0.0.1:7419"}, true},
		{"ipv4 loopback, wider 127/8", Endpoint{Network: "tcp", Address: "127.0.0.53:7419"}, true},
		{"ipv6 loopback", Endpoint{Network: "tcp", Address: "[::1]:7419"}, true},

		// The ones that matter: all-interfaces is NOT loopback, however
		// often it is reached over loopback in practice.
		{"ipv4 all interfaces", Endpoint{Network: "tcp", Address: "0.0.0.0:7419"}, false},
		{"ipv6 all interfaces", Endpoint{Network: "tcp", Address: "[::]:7419"}, false},
		{"routable lan address", Endpoint{Network: "tcp", Address: "192.168.1.10:7419"}, false},
		{"routable public address", Endpoint{Network: "tcp", Address: "165.232.128.152:7419"}, false},

		// Conservative on anything unparseable: withhold rather than guess.
		{"missing port", Endpoint{Network: "tcp", Address: "127.0.0.1"}, false},
		{"empty address", Endpoint{Network: "tcp", Address: ""}, false},
		// A hostname is not resolved — config validation rejects a
		// non-IP server.bind when a port is set, so this cannot arise from
		// a valid config, and guessing would mean trusting DNS.
		{"hostname is not treated as loopback", Endpoint{Network: "tcp", Address: "localhost:7419"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.ep.HostLocalOnly(); got != tc.want {
				t.Errorf("Endpoint{%s, %q}.HostLocalOnly() = %v, want %v",
					tc.ep.Network, tc.ep.Address, got, tc.want)
			}
		})
	}
}

// IsLocal stays unix-only; HostLocalOnly is the broader predicate. A loopback
// TCP bind is the case that separates them, and conflating the two would
// silently change what IsLocal's existing callers mean.
func TestEndpoint_HostLocalOnlyIsBroaderThanIsLocal(t *testing.T) {
	t.Parallel()
	lo := Endpoint{Network: "tcp", Address: "127.0.0.1:7419"}
	if lo.IsLocal() {
		t.Error("IsLocal() must stay unix-only")
	}
	if !lo.HostLocalOnly() {
		t.Error("HostLocalOnly() must accept a loopback TCP bind")
	}
}
