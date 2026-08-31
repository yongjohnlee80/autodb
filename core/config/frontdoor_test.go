package config

import (
	"errors"
	"strings"
	"testing"
)

func fdCfg(mut func(*Config)) Config {
	c := Default()
	c.FrontDoor.Enabled = true
	c.FrontDoor.TLSCertFile = "/etc/autodb/cert.pem"
	c.FrontDoor.TLSKeyFile = "/etc/autodb/key.pem"
	c.FrontDoor.TLSHostNames = []string{"autodb.example.com"}
	c.Exec.PoolMaxConns = 10
	c.FrontDoor.ReservedHeadroom = 4
	if mut != nil {
		mut(&c)
	}
	return c
}

// A disabled front door is not asked to hold a certificate. Refusing to start
// over an unused section would be a validator enforcing a feature nobody
// asked for.
func TestFrontDoor_DisabledIsNotValidated(t *testing.T) {
	t.Parallel()

	c := Default() // Enabled false, no TLS material, no bind of its own
	if err := c.validate(); err != nil {
		t.Fatalf("a default config with the front door off failed validation: %v", err)
	}
	// And the defaults are still the ADR's, so enabling it is one keystroke
	// rather than a research exercise.
	if c.FrontDoor.ReservedHeadroom != DefaultReservedHeadroom {
		t.Errorf("reserved_headroom default = %d, want %d",
			c.FrontDoor.ReservedHeadroom, DefaultReservedHeadroom)
	}
	if c.FrontDoor.Bind == "" {
		t.Error("bind has no default; enabling the front door would need two decisions, not one")
	}
}

// The §11 wiring cell (converted from the direct-cell shape below): the
// front-door refusal must be observable THROUGH Load. Every cell in
// TestFrontDoor_Validation calls validate() on a hand-built struct, so all
// of them stay green if Load stops calling validate() at all — the severed
// seam that shipped PR #22's transport check unwired. This cell dies with
// that call deleted (verified: `return cfg, cfg.validate()` → `return cfg,
// nil` in Load fails only this cell; the table stays green). The table
// below keeps per-message coverage.
func TestFrontDoor_ValidationIsWiredThroughLoad(t *testing.T) {
	t.Parallel()

	// Positive control through the same entry: a well-formed front-door
	// file is accepted, so the refusal below is a real one.
	mustLoad(t, `
[frontdoor]
enabled = true
tls_cert_file = "/etc/autodb/cert.pem"
tls_key_file = "/etc/autodb/key.pem"
tls_host_names = ["autodb.example.com"]
reserved_headroom = 4

[exec]
pool_max_conns = 10
`)

	msg := loadInvalid(t, "[frontdoor]\nenabled = true\n")
	if !strings.Contains(msg, "tls_cert_file and tls_key_file") {
		t.Fatalf("refusal does not carry the operator message: %q", msg)
	}
}

func TestFrontDoor_Validation(t *testing.T) {
	t.Parallel()

	// Positive control: the shape an operator would actually write passes.
	if err := fdCfg(nil).validate(); err != nil {
		t.Fatalf("a well-formed front-door config was refused (%v); this test cannot observe "+
			"a real refusal either", err)
	}

	for _, tc := range []struct {
		name string
		mut  func(*Config)
		says string
	}{
		{"no TLS material at all", func(c *Config) {
			c.FrontDoor.TLSCertFile, c.FrontDoor.TLSKeyFile = "", ""
		}, "tls_cert_file and tls_key_file"},
		{"a certificate with no key", func(c *Config) { c.FrontDoor.TLSKeyFile = "" },
			"tls_cert_file and tls_key_file"},
		{"an empty bind", func(c *Config) { c.FrontDoor.Bind = "" }, "has no address"},
		{"a bind that is not host:port", func(c *Config) { c.FrontDoor.Bind = "5432" }, "host:port"},
		{"negative headroom", func(c *Config) { c.FrontDoor.ReservedHeadroom = -1 }, "cannot be negative"},

		// MF1. Without this the SAN check was skippable by OMISSION: an
		// enabled front door with no names configured ran zero name checks,
		// and the one check that cannot be deferred to the client silently
		// did not run. verify-full verifies the NAME, so the gap simply
		// reappears at every client as an error each reads as their own.
		{"no host names at all", func(c *Config) { c.FrontDoor.TLSHostNames = nil },
			"tls_host_names is empty"},
		{"a blank host name", func(c *Config) {
			c.FrontDoor.TLSHostNames = []string{"autodb.example.com", "  "}
		}, "is blank"},
		{"negative max_leases", func(c *Config) { c.FrontDoor.MaxLeases = -1 }, "cannot be negative"},

		// The two that matter most, because both are configurations that
		// LOOK fine and fail later, on someone else's connection.
		{"headroom that leaves nothing to lease", func(c *Config) {
			c.Exec.PoolMaxConns = 4
			c.FrontDoor.ReservedHeadroom = 4
		}, "no capacity to serve anyone"},
		{"max_leases above what the pool can supply", func(c *Config) {
			c.FrontDoor.MaxLeases = 9 // pool 10 - headroom 4 = 6
		}, "does not create capacity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := fdCfg(tc.mut).validate()
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the message does not explain the problem (%q missing): %v", tc.says, err)
			}
		})
	}
}

// The lease cap is DERIVED unless set, and derived in exactly one place.
// Two call sites computing it separately is how a validator and an admission
// gate come to disagree — and the disagreement surfaces as leases refused by
// a cap no configuration file mentions.
func TestFrontDoor_EffectiveMaxLeases(t *testing.T) {
	t.Parallel()

	fd := FrontDoor{ReservedHeadroom: 4}
	if got := fd.EffectiveMaxLeases(10); got != 6 {
		t.Errorf("unset max_leases with pool 10 and headroom 4 = %d, want the derived 6", got)
	}
	fd.MaxLeases = 2
	if got := fd.EffectiveMaxLeases(10); got != 2 {
		t.Errorf("an explicit lower max_leases = %d, want 2", got)
	}
	// A larger explicit value cannot reach here — validation refuses it —
	// but if it ever did, the derivation must not silently raise the cap.
	fd.MaxLeases = 0
	fd.ReservedHeadroom = 0
	if got := fd.EffectiveMaxLeases(10); got != 10 {
		t.Errorf("no headroom = %d, want the whole pool", got)
	}
}
