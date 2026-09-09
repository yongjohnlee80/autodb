package config

// THE SHIPPED DEFAULT PAIR COULD NOT HOLD BELOW 3 vCPU.
//
// DefaultPoolMaxConns() is 2 x NumCPU and the reserved headroom was a flat 4,
// so on a 1 vCPU host the two shipped defaults were 2 and 4: the front door
// would be enabled with LESS THAN NOTHING to serve anyone with. On 2 vCPU it
// was exactly nothing. Nobody had met it because install_frontdoor.sh always
// emits pool_max_conns explicitly, sized for the host — but an invariant
// enforced by whoever happens to write the file is not an invariant, and an
// operator who enables the front door by editing the config met it.
//
// THE POOL IS NEVER RAISED to fix this. exec.pool_max_conns is a claim on the
// TARGET database's connection budget, a number the operator sized against a
// server autodb does not own; inflating it to satisfy an internal reservation
// would take backends nobody granted and move the error into somebody else's
// production database at peak. A reservation may shrink to fit a pool. It may
// never grow the pool to fit itself.

import (
	"fmt"
	"strings"
	"testing"
)

// sized builds an enabled front door whose pool is what a machine of `cores`
// would get by default, with the DEFAULT headroom for that pool.
//
// The fixture is complete enough to reach the sizing check, and that is not a
// detail: the first version of this probe omitted tls_host_names, hit an
// earlier refusal, and returned INVALID for every row — including 16 vCPU. A
// probe in which nothing passes is not measuring the thing it was asked about,
// and the uniform result was the only tell.
func sized(cores int) Config {
	pool := 2 * cores
	return fdCfg(func(c *Config) {
		c.Exec.PoolMaxConns = pool
		c.FrontDoor.ReservedHeadroom = DefaultReservedHeadroom(pool)
	})
}

// THE SHIPPED DEFAULTS VALIDATE AT EVERY CORE COUNT, and the leases each one
// yields are stated so the change at 3 vCPU is visible rather than discovered.
//
// Red at 1 and 2 before the fix, which is the mutation.
func TestSizing_ShippedDefaultsHoldAtEveryCoreCount(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		cores, pool, headroom, leases int
	}{
		{1, 2, 1, 1},
		{2, 4, 2, 2},
		{3, 6, 3, 3},
		{4, 8, 4, 4},
		{8, 16, 4, 12},
		{16, 32, 4, 28},
	} {
		t.Run(fmt.Sprintf("%dvCPU", tc.cores), func(t *testing.T) {
			c := sized(tc.cores)
			if got := c.FrontDoor.ReservedHeadroom; got != tc.headroom {
				t.Errorf("headroom for a pool of %d = %d, want %d", tc.pool, got, tc.headroom)
			}
			if err := c.validate(); err != nil {
				t.Fatalf("the SHIPPED defaults do not validate on a %d vCPU host "+
					"(pool %d, headroom %d): %v", tc.cores, tc.pool, c.FrontDoor.ReservedHeadroom, err)
			}
			if got := c.FrontDoor.EffectiveMaxLeases(tc.pool); got != tc.leases {
				t.Errorf("leases = %d, want %d — the number of clients a %d vCPU host "+
					"can actually serve", got, tc.leases, tc.cores)
			}
		})
	}
}

// A POOL OF ONE RESERVES NOTHING, and is a degraded install rather than an
// invalid one.
//
// A machine that can hold one connection cannot both reserve and serve, so the
// honest answer is that the front door gets it. Stated as its own case because
// integer division would otherwise decide it silently.
func TestSizing_APoolOfOneReservesNothing(t *testing.T) {
	t.Parallel()

	if got := DefaultReservedHeadroom(1); got != 0 {
		t.Errorf("headroom for a pool of 1 = %d, want 0", got)
	}
	c := fdCfg(func(c *Config) {
		c.Exec.PoolMaxConns = 1
		c.FrontDoor.ReservedHeadroom = DefaultReservedHeadroom(1)
	})
	if err := c.validate(); err != nil {
		t.Errorf("a pool of one is a degraded install, not an invalid one: %v", err)
	}
	if got := c.FrontDoor.EffectiveMaxLeases(1); got != 1 {
		t.Errorf("leases = %d, want 1", got)
	}
}

// THE DERIVATION NEVER RAISES THE POOL. The positive control for the paragraph
// at the top of this file: whatever the headroom does, the pool is the
// operator's number.
func TestSizing_TheDerivationNeverRaisesThePool(t *testing.T) {
	t.Parallel()

	for _, pool := range []int{1, 2, 3, 4, 8, 32} {
		if h := DefaultReservedHeadroom(pool); h >= pool && pool > 1 {
			t.Errorf("headroom for a pool of %d is %d, which leaves nothing to serve with",
				pool, h)
		}
	}
	// And through Load, where the derivation actually happens: an explicit
	// pool comes out unchanged.
	c := mustLoad(t, `
[exec]
pool_max_conns = 3

[frontdoor]
enabled = true
tls_cert_file = "/etc/autodb/cert.pem"
tls_key_file = "/etc/autodb/key.pem"
tls_host_names = ["autodb.example.com"]
`)
	if c.Exec.PoolMaxConns != 3 {
		t.Errorf("pool_max_conns = %d, want the operator's 3 — autodb does not get to "+
			"claim more of the target's connection budget than it was given",
			c.Exec.PoolMaxConns)
	}
	if c.FrontDoor.ReservedHeadroom != 1 {
		t.Errorf("headroom = %d, want min(4, 3/2) = 1", c.FrontDoor.ReservedHeadroom)
	}
}

// THE FOUR PROVENANCE CASES.
//
// The asymmetry in row 3 is deliberate: an explicit reserved_headroom is an
// INSTRUCTION, and silently reducing it would be autodb overriding a written
// decision — the class of behaviour this ADR is about. It is the DEFAULT that
// learns to fit; an explicit value gets an error instead.
func TestSizing_ProvenanceCases(t *testing.T) {
	t.Parallel()

	const fdBlock = `
enabled = true
tls_cert_file = "/etc/autodb/cert.pem"
tls_key_file = "/etc/autodb/key.pem"
tls_host_names = ["autodb.example.com"]
`

	t.Run("both default", func(t *testing.T) {
		// Neither key present: the pair can no longer disagree, whatever this
		// machine's core count is.
		c := mustLoad(t, "[frontdoor]"+fdBlock)
		pool := c.Exec.PoolMaxConns
		if want := DefaultReservedHeadroom(pool); c.FrontDoor.ReservedHeadroom != want {
			t.Errorf("headroom = %d, want the derived %d for a pool of %d",
				c.FrontDoor.ReservedHeadroom, want, pool)
		}
	})

	t.Run("pool explicit, headroom default", func(t *testing.T) {
		// THE INSTALLER'S CONFIG, byte for byte: it emits pool_max_conns = 8
		// and no headroom. It must be as valid as it is today, with the same
		// headroom of 4 — a fix that changed what the installer produces would
		// be a migration, not a fix.
		c := mustLoad(t, "[exec]\npool_max_conns = 8\n\n[frontdoor]"+fdBlock)
		if c.FrontDoor.ReservedHeadroom != 4 {
			t.Errorf("headroom = %d, want 4 for the installer's pool of 8",
				c.FrontDoor.ReservedHeadroom)
		}
		if got := c.FrontDoor.EffectiveMaxLeases(8); got != 4 {
			t.Errorf("leases = %d, want 4", got)
		}
	})

	t.Run("pool default, headroom explicit and impossible", func(t *testing.T) {
		// An explicit headroom is honoured, never reduced — so on a small host
		// this is an ERROR, and the message must say the pool was a DEFAULT.
		// An operator who set one number must not be shown two they never
		// typed as though they had chosen both.
		// NO [exec] SECTION: the pool must really be the default, or this
		// subtest is the both-explicit row wearing the wrong name — which is
		// what the first version of it was. The headroom is large enough to be
		// impossible against any machine's default pool, so the case is
		// reachable without pinning this host's core count.
		src := "[frontdoor]\nreserved_headroom = 1000" + fdBlock
		msg := loadInvalid(t, src)
		if !strings.Contains(msg, "reserved_headroom") {
			t.Errorf("the error does not name the key the operator set: %s", msg)
		}
		// PROVENANCE, which is what the old message got wrong: it read as
		// though the operator had chosen both numbers. Here they chose one.
		if !strings.Contains(msg, "1000 (which you set)") {
			t.Errorf("the headroom is not named as the operator's own: %s", msg)
		}
		if !strings.Contains(msg, "autodb's default") {
			t.Errorf("the pool is not named as a default: %s", msg)
		}
	})

	t.Run("both explicit and impossible", func(t *testing.T) {
		// Exactly today's refusal. The fix must not make a bad explicit
		// configuration acceptable.
		src := "[exec]\npool_max_conns = 4\n\n[frontdoor]\nreserved_headroom = 4" + fdBlock
		msg := loadInvalid(t, src)
		if !strings.Contains(msg, "no capacity to serve") {
			t.Errorf("the refusal lost its wording: %s", msg)
		}
		// BOTH the operator's, so neither may be called a default — that would
		// be the same misdirection pointed the other way.
		if strings.Contains(msg, "autodb's default") {
			t.Errorf("a value the operator set is described as a default: %s", msg)
		}
		if n := strings.Count(msg, "which you set"); n != 2 {
			t.Errorf("%d of the two explicit values are named as the operator's: %s", n, msg)
		}
	})
}

// PROVENANCE COMES FROM THE DECODER, NEVER FROM EQUALITY WITH THE DEFAULT.
//
// This is the case that fails if anyone implements provenance by comparing the
// value to its default: an operator who writes `reserved_headroom = 4` HAS set
// it, and telling them they did not — in the one message whose job is to say
// whose number is whose — would make the diagnostic wrong in exactly the way
// this ADR is about.
func TestSizing_AKeySetToItsDefaultIsStillSet(t *testing.T) {
	t.Parallel()

	// pool 8 with an EXPLICIT headroom of 4, which is also what the derivation
	// would have produced. Provenance must still record it as set.
	c := mustLoad(t, `
[exec]
pool_max_conns = 8

[frontdoor]
enabled = true
reserved_headroom = 4
tls_cert_file = "/etc/autodb/cert.pem"
tls_key_file = "/etc/autodb/key.pem"
tls_host_names = ["autodb.example.com"]
`)
	if !c.wasSet("frontdoor", "reserved_headroom") {
		t.Error("a key written explicitly to its own default value is reported as unset; " +
			"provenance is being inferred by equality rather than read from the decoder")
	}
	if !c.wasSet("exec", "pool_max_conns") {
		t.Error("an explicitly written pool_max_conns is reported as unset")
	}
	// And a key that really is absent is absent.
	if c.wasSet("frontdoor", "max_leases") {
		t.Error("a key that appears nowhere in the file is reported as set")
	}
}

// A CONFIG BUILT IN GO HAS NO PROVENANCE — that is UNKNOWN, not "defaulted".
//
// Default() and every programmatic caller bypass the decoder, so nothing
// observed which values a person chose. The message must not claim otherwise;
// with no provenance it names the values and the arithmetic without asserting
// who chose them.
func TestSizing_AGoBuiltConfigHasNoProvenanceClaims(t *testing.T) {
	t.Parallel()

	c := Default()
	if c.provenanceKnown() {
		t.Error("a Config built in Go claims to know where its values came from")
	}
	if c.wasSet("exec", "pool_max_conns") {
		t.Error("a Go-built Config reports a key as operator-set")
	}

	// And the refusal it produces says neither "you set" nor "the default":
	// it has no basis for either.
	bad := fdCfg(func(c *Config) {
		c.Exec.PoolMaxConns = 2
		c.FrontDoor.ReservedHeadroom = 4
	})
	err := bad.validate()
	if err == nil {
		t.Fatal("pool 2 with headroom 4 was accepted")
	}
	if strings.Contains(err.Error(), "you set") || strings.Contains(err.Error(), "the default") {
		t.Errorf("a Config with no provenance made a provenance claim: %v", err)
	}
}
