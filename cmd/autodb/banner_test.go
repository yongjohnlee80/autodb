package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// The two bind classes must produce DIFFERENT banners. A banner that ignored
// its address entirely would satisfy any single-address assertion, so each case
// also asserts the OTHER class's wording is ABSENT — the decoy half.
func TestCleartextBanner_BindClass(t *testing.T) {
	const loopbackPhrase = "loopback only"
	const offHostPhrase = "REACHABLE OFF-HOST"

	cases := []struct {
		name     string
		addr     string
		loopback bool
	}{
		{"ipv4 loopback", "127.0.0.1:5433", true},
		{"ipv4 loopback, other 127/8 address", "127.0.0.9:5433", true},
		{"ipv6 loopback", "[::1]:5433", true},
		{"wildcard bind", "0.0.0.0:5433", false},
		{"ipv6 wildcard", "[::]:5433", false},
		{"routable address", "10.4.1.7:5433", false},
		// Fail CLOSED: an address this cannot parse must not be described as
		// the reassuring case. The alarming description is the safe default
		// because the cost of the two mistakes is not symmetric.
		{"unparseable", "some-unix-socket", false},
		{"host name rather than an ip", "db.internal:5433", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cleartextBanner(tc.addr, 0)
			want, unwanted := offHostPhrase, loopbackPhrase
			if tc.loopback {
				want, unwanted = loopbackPhrase, offHostPhrase
			}
			if !strings.Contains(got, want) {
				t.Errorf("banner for %q does not say %q:\n%s", tc.addr, want, got)
			}
			if strings.Contains(got, unwanted) {
				t.Errorf("banner for %q wrongly says %q:\n%s", tc.addr, unwanted, got)
			}
			if !strings.Contains(got, tc.addr) {
				t.Errorf("banner does not name the address %q:\n%s", tc.addr, got)
			}
		})
	}
}

// The count is the part an operator acts on, and each of the four states has to
// read differently. The unknown state is the one worth guarding: a naive
// implementation prints "-1 cleartext debugging tokens exist", which is both
// wrong and — because it looks like a bug rather than a warning — ignorable.
func TestCleartextBanner_TokenCount(t *testing.T) {
	cases := []struct {
		name   string
		n      int
		want   string
		unwant []string
	}{
		{"none", 0, "No cleartext debugging tokens exist", []string{"RIGHT NOW"}},
		{"one", 1, "1 cleartext debugging token exists", []string{"No cleartext debugging tokens"}},
		{"several", 7, "7 cleartext debugging tokens exist", []string{"No cleartext debugging tokens"}},
		{"unknown", -1, "COULD NOT COUNT", []string{"-1", "No cleartext debugging tokens exist"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cleartextBanner("127.0.0.1:5433", tc.n)
			if !strings.Contains(got, tc.want) {
				t.Errorf("count %d: banner lacks %q:\n%s", tc.n, tc.want, got)
			}
			for _, u := range tc.unwant {
				if strings.Contains(got, u) {
					t.Errorf("count %d: banner wrongly contains %q:\n%s", tc.n, u, got)
				}
			}
		})
	}
}

// A warning that does not say how to stop the condition is a warning people
// learn to scroll past.
func TestCleartextBanner_NamesTheOffSwitch(t *testing.T) {
	got := cleartextBanner("0.0.0.0:5433", 2)
	for _, want := range []string{
		"WITHOUT TLS",
		"frontdoor.insecure_disable_tls",
		"restarting",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("banner lacks %q:\n%s", want, got)
		}
	}
}

// countDebugTokens claims the tokens it counts are "usable RIGHT NOW", so it
// has to mean what the auth path means: VerifyPAT refuses both a revoked token
// and an expired one.
//
// The cell asserts a POSITIVE CONTROL first — that the unfiltered row set holds
// three non-revoked debug rows — so a countDebugTokens that returned 1 by
// failing its query, or by counting nothing at all, cannot pass as though the
// filters were doing the work.
func TestCountDebugTokens_ExcludesRevokedAndExpired(t *testing.T) {
	ctx := context.Background()
	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	defer store.Close()

	now := time.Unix(1_700_000_000, 0)
	// The PAT rows carry a user FK, so the owner has to exist before them.
	uid, err := store.Users.OnCtx(ctx).
		Set(meta.UserName, "banner-owner").
		Set(meta.UserRole, "admin").
		Set(meta.UserPassHash, []byte("x")).
		Set(meta.UserCreatedAt, now.Unix()).
		Set(meta.UserUpdatedAt, now.Unix()).
		Insert()
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	insert := func(selector string, debug, revoked int64, expires time.Time) {
		t.Helper()
		if _, err := store.PATs.OnCtx(ctx).
			Set(meta.PATSelector, selector).
			Set(meta.PATSecretHash, "hash-"+selector).
			Set(meta.PATUserID, uid).
			Set(meta.PATName, "tok-"+selector).
			Set(meta.PATAllowedIPs, "203.0.113.4/32").
			Set(meta.PATConnID, int64(1)).
			Set(meta.PATDebugCleartext, debug).
			Set(meta.PATRevoked, revoked).
			Set(meta.PATCreatedAt, now.Unix()-60).
			Set(meta.PATExpiresAt, expires.Unix()).
			Insert(); err != nil {
			t.Fatalf("insert %s: %v", selector, err)
		}
	}
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)

	insert("aaaaaaaaaa", 1, 0, future) // the only usable one
	insert("bbbbbbbbbb", 1, 0, past)   // expired
	insert("cccccccccc", 1, 1, future) // revoked
	insert("dddddddddd", 0, 0, future) // not a debug token
	insert("eeeeeeeeee", 1, 0, now)    // expires exactly now: VerifyPAT refuses
	// it (now >= expires_at), so must not count

	// POSITIVE CONTROL: without the expiry and revocation filters the answer
	// would be 3, and without the debug filter it would be more still. If this
	// assertion fails the rest of the cell is measuring an empty table.
	raw, err := store.PATs.OnCtx(ctx).With(meta.PATDebugCleartext, int64(1)).Count()
	if err != nil {
		t.Fatalf("control count: %v", err)
	}
	if raw != 4 {
		t.Fatalf("control: want 4 debug rows in the table, got %d", raw)
	}

	if got := countDebugTokens(ctx, store, now); got != 1 {
		t.Errorf("countDebugTokens = %d, want 1 (revoked, expired, just-expired and non-debug rows all excluded)", got)
	}
	// And it MOVES with time rather than being a constant: one hour on, the
	// only live token has expired too.
	if got := countDebugTokens(ctx, store, future.Add(time.Second)); got != 0 {
		t.Errorf("countDebugTokens after expiry = %d, want 0", got)
	}
}
