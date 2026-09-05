package frontdoor

import (
	"sort"
	"testing"
)

// dnsPermitted's own contract, exercised directly.
//
// WHY THIS FILE EXISTS, stated plainly because the alternative reading is that
// it was written to justify a line. Mutating away the normalisation of the
// `name` argument left every end-to-end certgen cell green: splitHostNames
// normalises before the only production caller reaches this function, so from
// outside the package that half of the predicate is unreachable. By
// [[validate-the-verifier]] a guard whose mutation changes no outcome should be
// DELETED rather than kept for safety, and that rule is right.
//
// It is kept because normalisation is this predicate's CONTRACT rather than a
// guard against a caller's mistake. The `permitted` side is read back from a
// CERTIFICATE FILE — written by a previous version of this code, or by another
// tool entirely — so one side genuinely must normalise, and a predicate that
// normalised one side and not the other would answer differently depending on
// which argument carried the oddity. The contract is "does this constraint set
// permit this name, per RFC 5280", and that is what is tested here.
func TestDNSPermitted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		permitted []string
		want      bool
		why       string
	}{
		{"example.com", []string{"example.com"}, true, "the domain itself"},
		{"db.example.com", []string{"example.com"}, true, "a subdomain"},
		{"a.b.example.com", []string{"example.com"}, true, "a deeper subdomain"},
		// THE PERMISSIVE FAILURE, and the only direction that is dangerous: a
		// bare strings.HasSuffix admits this for a constraint of example.com.
		{"evilexample.com", []string{"example.com"}, false, "not a subdomain"},
		{"example.com.evil.net", []string{"example.com"}, false, "present, but not as a suffix"},
		{"elsewhere.net", []string{"example.com"}, false, "a different domain"},
		// Normalisation, on EACH side independently, which is the half no
		// end-to-end cell can reach.
		{"DB.Example.COM", []string{"example.com"}, true, "the name is uppercase"},
		{"db.example.com", []string{"EXAMPLE.COM"}, true, "the CONSTRAINT is uppercase"},
		{"db.example.com.", []string{"example.com"}, true, "the name is fully qualified"},
		{"db.example.com", []string{"example.com."}, true, "the CONSTRAINT is fully qualified"},
		{"DB.Example.COM.", []string{"EXAMPLE.COM."}, true, "both are"},
		// RFC 5280 writes a dNSName subtree with a leading dot in some
		// profiles; it means the same subtree.
		{"db.example.com", []string{".example.com"}, true, "a leading-dot constraint"},
		// An empty constraint must not match everything. This is the shape
		// that turns a name-constrained CA back into an unconstrained one.
		{"anything.example.com", []string{""}, false, "an empty constraint permits nothing"},
		{"anything.example.com", []string{"."}, false, "a bare dot permits nothing"},
		// The degenerate pair, which is where the empty-constraint guard
		// actually bites: without it "" == "" makes an empty constraint permit
		// an empty name, and a predicate that answers YES to "is nothing
		// permitted" is one an unconstrained CA can hide behind.
		{"", []string{""}, false, "nothing is not a permitted name"},
		{"", []string{"example.com"}, false, "an empty name against a real constraint"},
		// Several constraints: any one may permit.
		{"db.other.net", []string{"example.com", "other.net"}, true, "the second constraint"},
		{"db.third.org", []string{"example.com", "other.net"}, false, "neither constraint"},
	}
	for _, tc := range cases {
		got := dnsPermitted(tc.name, tc.permitted)
		if got != tc.want {
			t.Errorf("dnsPermitted(%q, %v) = %v, want %v (%s)",
				tc.name, tc.permitted, got, tc.want, tc.why)
		}
	}
}

// THE KEY-AGREEMENT SEAM between the two halves of the requested/convenience
// policy, which a reviewer named and which is now closed by construction.
//
// constraintsPermit looks up requested[h] where h came out of splitHostNames'
// dns/ips lists. If the key recorded in `requested` were computed by a second
// path, and the two ever normalised differently, a name the operator ASKED FOR
// would miss the lookup and be silently downgraded to a convenience — DROPPED
// instead of REFUSED. That is the exact inversion of the policy, in the
// direction that ISSUES a certificate rather than refusing one.
//
// The first version did compute it twice: the same bracket strip, the same
// ParseIP, the same normalizeDNSName, written in two places. It is now recorded
// inside add() from the same variable that becomes the list entry, so the two
// cannot disagree. This cell is the behavioural guard over that, walking the
// spellings the normalisation work was about.
func TestSplitHostNames_RequestedKeysMatchTheListEntries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string // the key BOTH halves must agree on
	}{
		{"db.example.com", "db.example.com"},
		{"DB.Example.COM", "db.example.com"},
		{"db.example.com.", "db.example.com"},
		{"  db.example.com  ", "db.example.com"},
		{"DB.Example.COM.", "db.example.com"},
		{"10.0.0.5", "10.0.0.5"},
		{"[::1]", "::1"},
		{"::1", "::1"},
		{"[2001:db8::1]", "2001:db8::1"},
		// Canonicalised by net.IP.String, which is the form the list carries.
		{"2001:0db8:0000:0000:0000:0000:0000:0001", "2001:db8::1"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			dns, ips, requested, err := splitHostNames([]string{tc.in})
			if err != nil {
				t.Fatalf("splitHostNames(%q): %v", tc.in, err)
			}
			if !requested[tc.want] {
				t.Fatalf("requested does not contain %q for input %q; the operator's name would "+
					"be treated as a convenience and DROPPED instead of REFUSED (got keys %v)",
					tc.want, tc.in, sortedKeys(requested))
			}
			// And the key is one the LIST actually carries, which is what
			// constraintsPermit will look up.
			var found bool
			for _, d := range dns {
				if d == tc.want {
					found = true
				}
			}
			for _, ip := range ips {
				if ip.String() == tc.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("%q is in requested but is not an entry in dns=%v ips=%v; the lookup in "+
					"constraintsPermit would miss it", tc.want, dns, ips)
			}
		})
	}

	// THE DECOY. The convenience names must NOT be marked requested, or the
	// whole distinction collapses and every loopback name becomes a refusal.
	t.Run("the loopback set is not requested", func(t *testing.T) {
		_, _, requested, err := splitHostNames([]string{"db.example.com"})
		if err != nil {
			t.Fatal(err)
		}
		for _, conv := range []string{"localhost", "127.0.0.1", "::1"} {
			if requested[conv] {
				t.Errorf("%q was recorded as requested; it is a convenience this function adds, "+
					"and marking it requested turns a droppable name into a hard refusal", conv)
			}
		}
	})

	// But an operator who names one EXPLICITLY does get it as requested, so
	// they can insist and be told why rather than have it silently dropped.
	t.Run("an explicit loopback name IS requested", func(t *testing.T) {
		_, _, requested, err := splitHostNames([]string{"db.example.com", "localhost", "127.0.0.1"})
		if err != nil {
			t.Fatal(err)
		}
		for _, explicit := range []string{"localhost", "127.0.0.1"} {
			if !requested[explicit] {
				t.Errorf("%q was named explicitly but is not recorded as requested", explicit)
			}
		}
		if requested["::1"] {
			t.Error("::1 was not named and must not be requested")
		}
	})
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
