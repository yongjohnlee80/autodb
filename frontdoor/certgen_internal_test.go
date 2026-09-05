package frontdoor

import "testing"

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
