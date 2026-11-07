package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func fdCfg(mut func(*Config)) Config {
	c := Default()
	c.FrontDoor.Enabled = true
	c.FrontDoor.TLSCertFile = "/etc/autodb/cert.pem"
	c.FrontDoor.TLSKeyFile = "/etc/autodb/key.pem"
	c.FrontDoor.TLSHostNames = []string{"autodb.example.com"}
	c.Exec.PoolMaxConns = 10
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
	// Enabling it is one keystroke rather than a research exercise: the
	// socket budget comes from connection pools, not from this file.
	if c.FrontDoor.Bind == "" {
		t.Error("bind has no default; enabling the front door would need two decisions, not one")
	}
}

// The wiring cell (converted from the direct-cell shape below): the
// front-door refusal must be observable THROUGH Load. Every cell in
// TestFrontDoor_Validation calls validate() on a hand-built struct, so all
// of them stay green if Load stops calling validate() at all — the severed
// seam that shipped the transport check unwired. This cell dies with
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

		// Without this the SAN check was skippable by OMISSION: an
		// enabled front door with no names configured ran zero name checks,
		// and the one check that cannot be deferred to the client silently
		// did not run. verify-full verifies the NAME, so the gap simply
		// reappears at every client as an error each reads as their own.
		{"no host names at all", func(c *Config) { c.FrontDoor.TLSHostNames = nil },
			"tls_host_names is empty"},
		{"a blank host name", func(c *Config) {
			c.FrontDoor.TLSHostNames = []string{"autodb.example.com", "  "}
		}, "is blank"},
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

// THE RETIRED BUDGET KEYS LOAD, WITH A WARNING THAT NAMES WHAT REPLACED THEM.
//
// Socket budgets moved to connection pools in the meta store. A file written
// for an earlier release still sets these, and refusing it would stop an
// upgrade over a number that now does nothing.
func TestFrontDoor_RetiredBudgetKeysLoadWithAWarning(t *testing.T) {
	t.Parallel()

	c := mustLoad(t, `
[frontdoor]
enabled = true
tls_cert_file = "/etc/autodb/cert.pem"
tls_key_file = "/etc/autodb/key.pem"
tls_host_names = ["autodb.example.com"]
reserved_headroom = 4
max_leases = 6

[exec]
max_target_conns = 25
`)
	for _, key := range []string{"exec.max_target_conns", "frontdoor.max_leases", "frontdoor.reserved_headroom"} {
		var said string
		for _, w := range c.Warnings() {
			if strings.Contains(w, key) {
				said = w
			}
		}
		if said == "" {
			t.Errorf("%s loaded silently; it has no effect now, and the operator must be told", key)
			continue
		}
		if !strings.Contains(said, "connection pool") {
			t.Errorf("the warning for %s doesn't say what replaced it: %q", key, said)
		}
	}
}

// An idle session must not outlive the pool's own idle reaping.
func TestFrontDoor_SessionIdleMustNotExceedPoolIdle(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		session, pool Duration
		wantErr       bool
	}{
		{"below", Duration(5 * time.Minute), Duration(10 * time.Minute), false},
		{"equal", Duration(10 * time.Minute), Duration(10 * time.Minute), false},
		{"above", Duration(11 * time.Minute), Duration(10 * time.Minute), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fdCfg(func(c *Config) {
				c.Exec.SessionIdleTimeout = tc.session
				c.Exec.PoolMaxConnIdleTime = tc.pool
			})
			err := c.validate()
			if tc.wantErr && err == nil {
				t.Error("a session bound above the pool's idle reaping must be refused")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected refusal: %v", err)
			}
		})
	}
}
