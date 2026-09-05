package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// The debug_cleartext mint gate (ADR-0086 §10, R7).
//
// It had NO cells at all in its first round, while the banners and the
// connection card that report the same mode carried thirteen mutation-proven
// assertions between them. A reviewer named the inversion: the presentation
// layer was proven to a high standard and the security core was not covered,
// which is the opposite of the priority [[security-core-hardening]] sets. Every
// ground below therefore gets a cell AND a decoy — the question is never only
// "does it refuse", it is "what else would it accept".

// cleartextSvc is a Service whose front door IS serving cleartext.
func cleartextSvc(t *testing.T, serving bool) (*Service, *meta.Store, *clock) {
	t.Helper()
	store, err := meta.Open(context.Background(), config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ck := &clock{t: time.Unix(1_800_000_000, 0)}
	s, err := New(store, WithNow(ck.Now), WithConfigAllowlist([]string{"127.0.0.1/32"}),
		WithServingCleartext(func() bool { return serving }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, store, ck
}

// grantConn gives a second user a grant on the fixture connection.
//
// mustFrontDoorConn grants only the caller who creates the row and returns
// early on every later call, so a cell with two identities needs this or the
// second one is refused by the GRANT gate and never reaches the one under test.
func grantConn(t *testing.T, s *Service, connID, userID int64) {
	t.Helper()
	if _, err := s.store.Grants.OnCtx(context.Background()).
		Set(meta.GrantUserID, userID).Set(meta.GrantConnID, connID).
		Set(meta.GrantRole, meta.RoleReader).
		Set(meta.GrantGrantedBy, userID).Set(meta.GrantCreatedAt, int64(1)).
		Insert(); err != nil {
		t.Fatalf("granting the fixture connection to %d: %v", userID, err)
	}
}

// GROUND 1 — admin only, with an EDITOR as the decoy.
//
// "Somebody without a token" is refused by every gate in this file and would
// prove nothing about this one. The interesting caller is a legitimate,
// authenticated user who may mint ordinary PATs all day and must not mint this
// one.
func TestDebugCleartextMint_AdminOnly(t *testing.T) {
	t.Parallel()
	s, _, _ := cleartextSvc(t, true)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()

	if _, err := s.CreateUser(ctx, rootTok, "eve", "eve-passphrase-long", meta.RoleEditor, testIP); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	eveTok, eveIdent, err := s.Login(ctx, "eve", "eve-passphrase-long", testIP)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	conn := patConn(t, s, rootTok)
	grantConn(t, s, conn, eveIdent.UserID())

	// POSITIVE CONTROL: the editor CAN mint an ordinary token against the same
	// connection. Without this the refusal below could be any unrelated denial
	// — a missing grant, a bad connection id — wearing the gate's name.
	if _, err := s.CreatePAT(ctx, eveTok, "eve-ordinary", conn, 0, nil, false); err != nil {
		t.Fatalf("an editor could not mint an ORDINARY token (%v); this cell cannot "+
			"observe the debug gate, only the failure that precedes it", err)
	}

	_, err = s.CreatePAT(ctx, eveTok, "eve-debug", conn, 0, []string{"203.0.113.4/32"}, true)
	if !errors.Is(err, ErrPATDebugCleartextRefused) {
		t.Fatalf("an EDITOR minted a cleartext debugging token: %v", err)
	}

	// And the admin is not refused for some unrelated reason — otherwise the
	// gate would look correct while refusing everyone.
	if _, err := s.CreatePAT(ctx, rootTok, "root-debug", conn, 0, []string{"203.0.113.4/32"}, true); err != nil {
		t.Fatalf("an ADMIN was refused too (%v); the gate refuses everybody, which is not "+
			"the same as refusing non-admins", err)
	}
}

// GROUND 2 — the mode must be on NOW.
//
// The failure this prevents is dormancy: tokens minted on a TLS-only install
// sit inert and ACTIVATE ALL AT ONCE the day somebody enables cleartext for an
// afternoon, each usable from addresses nobody vetted. Mint time is the only
// moment a dormant credential can be stopped.
func TestDebugCleartextMint_RefusedWhenNotServingCleartext(t *testing.T) {
	t.Parallel()
	s, _, _ := cleartextSvc(t, false)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()
	conn := patConn(t, s, rootTok)

	// CONTROL: an ordinary token mints fine here, so the refusal below is
	// about the debug flag and not about this fixture being broken.
	if _, err := s.CreatePAT(ctx, rootTok, "ordinary", conn, 0, nil, false); err != nil {
		t.Fatalf("an ordinary token could not be minted (%v); nothing below is observable", err)
	}

	_, err := s.CreatePAT(ctx, rootTok, "debug", conn, 0, []string{"203.0.113.4/32"}, true)
	if !errors.Is(err, ErrPATDebugCleartextRefused) {
		t.Fatalf("a debug token was minted on a TLS-only daemon: %v", err)
	}
	if !strings.Contains(err.Error(), "inert") {
		t.Errorf("the refusal does not explain the dormancy it prevents: %v", err)
	}
}

// The hook is absent — an install that never wired it.
//
// NIL MEANS FALSE is the whole safety of this gate: an unwired install must
// refuse to mint rather than mint freely. This is the one case where a
// mistake in the WIRING, not in the policy, decides whether credentials exist.
func TestDebugCleartextMint_UnwiredHookFailsClosed(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t) // no WithServingCleartext at all
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()
	conn := patConn(t, s, rootTok)

	if _, err := s.CreatePAT(ctx, rootTok, "ordinary", conn, 0, nil, false); err != nil {
		t.Fatalf("control: an ordinary token could not be minted (%v)", err)
	}
	_, err := s.CreatePAT(ctx, rootTok, "debug", conn, 0, []string{"203.0.113.4/32"}, true)
	if !errors.Is(err, ErrPATDebugCleartextRefused) {
		t.Fatalf("an install that never wired the cleartext hook minted a debug token "+
			"anyway: %v", err)
	}
}

// GROUND 3 — the list must be NARROW, not merely non-empty.
//
// The first version tested emptiness while claiming to mean "not from
// anywhere", and a reviewer minted a token with 0.0.0.0/0 — not empty, and
// meaning precisely from anywhere. The table walks both sides of the floor in
// both families, so a check that refused everything, or that refused only the
// literal string "0.0.0.0/0", fails here.
func TestDebugCleartextMint_RefusesBroadRanges(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		ips     []string
		refused bool
	}{
		{"the whole IPv4 internet", []string{"0.0.0.0/0"}, true},
		{"half of it", []string{"0.0.0.0/1"}, true},
		{"a /8", []string{"10.0.0.0/8"}, true},
		{"a /16", []string{"10.1.0.0/16"}, true},
		{"one address", []string{"203.0.113.4/32"}, false},
		{"an office subnet", []string{"203.0.113.0/24"}, false},
		{"the whole IPv6 internet", []string{"::/0"}, true},
		{"an ISP-sized block", []string{"2001:db8::/32"}, true},
		{"a site block", []string{"2001:db8::/48"}, true},
		{"one LAN", []string{"2001:db8::/64"}, false},
		{"one IPv6 host", []string{"2001:db8::1/128"}, false},
		// A broad entry hiding behind a narrow one. Checking only the first
		// entry would pass this while admitting the internet.
		{"a narrow entry followed by the internet", []string{"203.0.113.4/32", "0.0.0.0/0"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := cleartextSvc(t, true)
			rootTok, _ := mustBootstrap(t, s)
			ctx := context.Background()
			conn := patConn(t, s, rootTok)

			_, err := s.CreatePAT(ctx, rootTok, "debug", conn, 0, tc.ips, true)
			refused := errors.Is(err, ErrPATDebugCleartextRefused)
			if refused != tc.refused {
				t.Fatalf("allowed_ips %v: refused=%v want %v (err %v)", tc.ips, refused, tc.refused, err)
			}
			if !tc.refused && err != nil {
				t.Fatalf("allowed_ips %v was rejected for another reason: %v", tc.ips, err)
			}
		})
	}
}

// An empty list is still refused, and separately from breadth: the two grounds
// answer different mistakes and a single message for both would send an
// operator to change the wrong thing.
func TestDebugCleartextMint_RefusesEmptyList(t *testing.T) {
	t.Parallel()
	s, _, _ := cleartextSvc(t, true)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()

	_, err := s.CreatePAT(ctx, rootTok, "debug", patConn(t, s, rootTok), 0, nil, true)
	if !errors.Is(err, ErrPATDebugCleartextRefused) {
		t.Fatalf("a debug token with no allowed_ips was minted: %v", err)
	}
	if !strings.Contains(err.Error(), "anywhere") {
		t.Errorf("the refusal does not say what empty would have meant: %v", err)
	}
}

// A minted debug token is recorded AS ONE.
//
// Without a distinct audit action an investigation cannot separate a debug
// credential from an ordinary one without reading flags off rows, and the
// question "what did we activate that afternoon" has no answer.
func TestDebugCleartextMint_HasItsOwnAuditAction(t *testing.T) {
	t.Parallel()
	s, store, _ := cleartextSvc(t, true)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()
	conn := patConn(t, s, rootTok)

	// The ordinary token INHERITS its owner's admission (an empty list), which
	// is what empty means everywhere except the debug branch. A debug token's
	// list is a perimeter of its own — the relaxation this gate compensates
	// for — and that is exactly why the two must not share a message.
	if _, err := s.CreatePAT(ctx, rootTok, "ordinary", conn, 0, nil, false); err != nil {
		t.Fatalf("CreatePAT(ordinary): %v", err)
	}
	if _, err := s.CreatePAT(ctx, rootTok, "debug", conn, 0, []string{"203.0.113.4/32"}, true); err != nil {
		t.Fatalf("CreatePAT(debug): %v", err)
	}
	if n := auditCount(t, store, "pat_created_debug_cleartext"); n != 1 {
		t.Errorf("pat_created_debug_cleartext rows = %d, want 1", n)
	}
	// The DECOY: the ordinary token must NOT have landed under the debug
	// action, and the debug one must not have landed under the ordinary one.
	if n := auditCount(t, store, "pat_created"); n != 1 {
		t.Errorf("pat_created rows = %d, want 1 — the two actions are not distinct", n)
	}
}

// And the flag reaches the row. A gate that refuses correctly while storing
// nothing would leave the banner counting zero debug tokens on an install that
// has one.
func TestDebugCleartextMint_PersistsTheFlag(t *testing.T) {
	t.Parallel()
	s, store, _ := cleartextSvc(t, true)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()
	conn := patConn(t, s, rootTok)

	if _, err := s.CreatePAT(ctx, rootTok, "ordinary", conn, 0, nil, false); err != nil {
		t.Fatalf("CreatePAT(ordinary): %v", err)
	}
	if _, err := s.CreatePAT(ctx, rootTok, "debug", conn, 0, []string{"203.0.113.4/32"}, true); err != nil {
		t.Fatalf("CreatePAT(debug): %v", err)
	}
	n, err := store.PATs.OnCtx(ctx).With(meta.PATDebugCleartext, int64(1)).Count()
	if err != nil {
		t.Fatalf("counting debug tokens: %v", err)
	}
	if n != 1 {
		t.Errorf("debug_cleartext rows = %d, want exactly 1 (the ordinary token must not "+
			"carry the flag, and the debug one must)", n)
	}
}
