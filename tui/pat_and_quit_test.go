package tui

import (
	"net/netip"
	"testing"

	"github.com/yongjohnlee80/golib/tui"
)

// key builds the press this UI actually receives.
func key(s string) tui.Event { return tui.KeyEvent{Text: s} }

// TestLeaderResolve_BoundEntryBeatsDismiss is the regression this change was
// most likely to cause. The SPC menu binds `q` to "focus query editor"; a
// dismiss-first rule would have deleted that command silently, and nothing
// else in the suite would have noticed.
func TestLeaderResolve_BoundEntryBeatsDismiss(t *testing.T) {
	spc := []leaderEntry{
		{'r', "run query", func() {}},
		{'q', "focus query editor", func() {}},
		{'t', "focus results", func() {}},
	}
	idx, dismiss := leaderResolve(spc, key("q"))
	if dismiss {
		t.Fatal("q dismissed the SPC menu, destroying the bound 'focus query editor'")
	}
	if idx != 1 {
		t.Fatalf("q resolved to entry %d, want 1 (focus query editor)", idx)
	}
}

// TestLeaderResolve_UnboundQDismisses is the feature: every menu and
// confirmation that does not bind `q` now closes on it, like Esc.
func TestLeaderResolve_UnboundQDismisses(t *testing.T) {
	confirm := []leaderEntry{
		{'y', "delete it", func() {}},
		{'n', "keep it", func() {}},
	}
	idx, dismiss := leaderResolve(confirm, key("q"))
	if !dismiss {
		t.Fatal("q did not dismiss a confirmation that binds no q")
	}
	if idx != -1 {
		t.Fatalf("q resolved to entry %d, want none", idx)
	}
}

// TestLeaderResolve_BoundKeysStillRun guards the ordinary path: adding the
// fallback must not have disturbed normal selection.
func TestLeaderResolve_BoundKeysStillRun(t *testing.T) {
	entries := []leaderEntry{{'y', "yes", func() {}}, {'n', "no", func() {}}}
	for _, tc := range []struct {
		press string
		want  int
	}{{"y", 0}, {"n", 1}} {
		idx, dismiss := leaderResolve(entries, key(tc.press))
		if dismiss || idx != tc.want {
			t.Fatalf("%q resolved to (%d, dismiss=%v), want (%d, false)",
				tc.press, idx, dismiss, tc.want)
		}
	}
}

// TestLeaderResolve_UnknownKeyDoesNothing — an unbound, non-q key must fall
// through so the table underneath still gets its motion keys.
func TestLeaderResolve_UnknownKeyDoesNothing(t *testing.T) {
	entries := []leaderEntry{{'y', "yes", func() {}}}
	if idx, dismiss := leaderResolve(entries, key("Z")); idx != -1 || dismiss {
		t.Fatalf("unknown key resolved to (%d, dismiss=%v), want (-1, false)", idx, dismiss)
	}
}

// TestLeaderResolve_KeyReleaseIsNotAPress — release events must not trigger
// an action, or a keypress would fire twice.
func TestLeaderResolve_KeyReleaseIsNotAPress(t *testing.T) {
	entries := []leaderEntry{{'y', "yes", func() {}}}
	ev := tui.KeyEvent{Text: "y", Kind: tui.KeyRelease}
	if idx, dismiss := leaderResolve(entries, ev); idx != -1 || dismiss {
		t.Fatalf("a key RELEASE resolved to (%d, dismiss=%v), want (-1, false)", idx, dismiss)
	}
}

// TestConfirmQuit_DoesNotBindQ pins the decision behind the quit modal: `q`
// is the key that OPENS it, so binding it as a choice would make `qq` an
// instant exit and defeat the confirmation. Leaving it unbound routes it to
// the dismiss fallback, making a double-tap a safe no-op.
//
// Asserted through leaderResolve rather than by reading the slice, so it
// tests the behaviour a user gets.
func TestConfirmQuit_DoesNotBindQ(t *testing.T) {
	quit := []leaderEntry{
		{'y', "quit", func() {}},
		{'n', "stay", func() {}},
	}
	idx, dismiss := leaderResolve(quit, key("q"))
	if idx != -1 {
		t.Fatalf("q is BOUND in the quit modal (entry %d) — qq would exit instantly", idx)
	}
	if !dismiss {
		t.Fatal("q in the quit modal neither ran a choice nor dismissed; it must cancel")
	}
}

// --- PAT allowed_ips subset validation ---------------------------------------

func TestParseCIDROrAddr_BareAddressBecomesHostPrefix(t *testing.T) {
	p, err := parseCIDROrAddr("10.1.2.3")
	if err != nil {
		t.Fatalf("bare address rejected: %v", err)
	}
	if p.Bits() != 32 {
		t.Fatalf("bare v4 address became /%d, want /32", p.Bits())
	}
	p6, err := parseCIDROrAddr("2001:db8::1")
	if err != nil {
		t.Fatalf("bare v6 address rejected: %v", err)
	}
	if p6.Bits() != 128 {
		t.Fatalf("bare v6 address became /%d, want /128", p6.Bits())
	}
}

func TestParseCIDROrAddr_RejectsNonsense(t *testing.T) {
	for _, s := range []string{"", "not-an-ip", "10.0.0.0/99", "10.0.0.256"} {
		if _, err := parseCIDROrAddr(s); err == nil {
			t.Fatalf("%q was accepted as an IP or CIDR", s)
		}
	}
}

// TestWithinAny_ContainmentNotEquality is the reason withinAny exists: a user
// allowed 10.0.0.0/8 should be able to restrict a token to a single host
// inside it. Equality matching would refuse that and force the user to widen
// their own allowlist to narrow a token — backwards.
func TestWithinAny_ContainmentNotEquality(t *testing.T) {
	own := []UserIPRow{{CIDR: "10.0.0.0/8"}}
	want, _ := parseCIDROrAddr("10.1.2.3")
	if !withinAny(want, own) {
		t.Fatal("10.1.2.3 rejected against an allowlist of 10.0.0.0/8")
	}
}

// TestWithinAny_RefusesOutside is the security half: allowed_ips must be a
// SUBSET of the user's own rows (ADR-0075 §4). A token may not reach an
// address its owner cannot.
func TestWithinAny_RefusesOutside(t *testing.T) {
	own := []UserIPRow{{CIDR: "10.0.0.0/8"}, {CIDR: "192.168.68.0/24"}}
	for _, s := range []string{"172.16.0.1", "192.168.69.1", "0.0.0.0/0"} {
		want, err := parseCIDROrAddr(s)
		if err != nil {
			t.Fatalf("fixture %q did not parse: %v", s, err)
		}
		if withinAny(want, own) {
			t.Fatalf("%q accepted though it is outside the user's allowlist", s)
		}
	}
}

// TestWithinAny_WiderPrefixIsNotContained catches the subtle direction error:
// a user allowed 10.1.0.0/16 must NOT be able to mint a token for 10.0.0.0/8.
// Contains() alone would pass here, because the /8 contains the /16's address
// — the Bits() comparison is what makes it a subset test.
// The fixture SHARES a base address on purpose. With own=10.0.0.0/16 and
// want=10.0.0.0/8, Contains(want.Addr()) is TRUE — 10.0.0.0 does sit inside
// the /16 — so only the Bits() comparison can reject it. An earlier version
// of this cell used 10.1.0.0/16, where Contains already returned false, and
// it passed with the guard deleted: it asserted the right thing about the
// wrong inputs and observed nothing.
func TestWithinAny_WiderPrefixIsNotContained(t *testing.T) {
	own := []UserIPRow{{CIDR: "10.0.0.0/16"}}
	want := netip.MustParsePrefix("10.0.0.0/8")
	if withinAny(want, own) {
		t.Fatal("a /8 was accepted against an allowlist of /16 — the token would " +
			"reach addresses its owner cannot")
	}
}

// TestWithinAny_EmptyAllowlistPermitsNothing — with no rows of their own, a
// user cannot pin a token to any address.
func TestWithinAny_EmptyAllowlistPermitsNothing(t *testing.T) {
	want := netip.MustParsePrefix("10.0.0.0/8")
	if withinAny(want, nil) {
		t.Fatal("an empty allowlist accepted a restriction")
	}
}

// TestWithinAny_SkipsUnparseableRows — a malformed stored row must not crash
// the check or, worse, be treated as permissive.
func TestWithinAny_SkipsUnparseableRows(t *testing.T) {
	own := []UserIPRow{{CIDR: "garbage"}, {CIDR: "10.0.0.0/8"}}
	inside, _ := parseCIDROrAddr("10.1.2.3")
	if !withinAny(inside, own) {
		t.Fatal("a malformed row stopped a legitimate match")
	}
	outside, _ := parseCIDROrAddr("172.16.0.1")
	if withinAny(outside, []UserIPRow{{CIDR: "garbage"}}) {
		t.Fatal("a malformed row was treated as permissive")
	}
}

func TestShortStamp(t *testing.T) {
	if got := shortStamp("2026-09-01T13:31:32Z"); got != "2026-09-01" {
		t.Fatalf("shortStamp trimmed to %q, want 2026-09-01", got)
	}
	// The wire uses words for "never"; those must survive intact.
	if got := shortStamp("never"); got != "never" {
		t.Fatalf("shortStamp mangled %q into %q", "never", got)
	}
	if got := shortStamp(""); got != "" {
		t.Fatalf("shortStamp turned empty into %q", got)
	}
}
