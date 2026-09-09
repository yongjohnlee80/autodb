package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// The mint path may add the caller's own allowlist rows once they have
// approved an exact set. These cells are the evidence matrix for that: what it
// adds, what it refuses, what it must never touch, and what it rolls back.

// patAuditCount counts audit rows by action, reading through the service's own
// store so a cell does not need the store handle threaded to it.
func patAuditCount(t *testing.T, s *Service, action string) int {
	t.Helper()
	n, err := s.store.Audit.OnCtx(context.Background()).With(meta.AuditAction, action).Count()
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

func seedRow(t *testing.T, s *Service, userID int64, cidr, label string) {
	t.Helper()
	if _, err := s.store.UserIPs.OnCtx(context.Background()).
		Set(meta.UIPUserID, userID).Set(meta.UIPCIDR, cidr).
		Set(meta.UIPLabel, label).Set(meta.UIPCreatedAt, int64(1)).Insert(); err != nil {
		t.Fatalf("seeding %s: %v", cidr, err)
	}
}

func rowsOf(t *testing.T, s *Service, userID int64) map[string]string {
	t.Helper()
	rows, err := s.store.UserIPs.OnCtx(context.Background()).
		With(meta.UIPUserID, userID).Select()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.CIDR] = r.Label
	}
	return out
}

// AN UNACKNOWLEDGED CALLER KEEPS TODAY'S REFUSAL.
//
// This is the fail-closed direction and the most important cell here: adding
// the acknowledgement must not make the ordinary path permissive. A caller
// that does not opt in cannot widen anything.
func TestMintAllowlist_WithoutApprovalTheSubsetRefusalStands(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	tok, ident := mustBootstrap(t, s)
	ctx := context.Background()
	conn := mustFrontDoorConn(t, s, ident.UserID())

	_, err := s.CreatePAT(ctx, tok, "no-approval", conn, 0, []string{"203.0.113.7/32"}, false, nil, testIP)
	if !errors.Is(err, ErrPATBadAllowedIPs) {
		t.Fatalf("an unapproved widening was not refused: %v", err)
	}
	if got := rowsOf(t, s, ident.UserID()); len(got) != 0 {
		t.Errorf("the refused mint added rows anyway: %v", got)
	}
	if n := patAuditCount(t, s, "user_ip_added"); n != 0 {
		t.Errorf("the refused mint wrote %d allowlist audit rows", n)
	}
}

// APPROVED: the rows, the token and the audits all land, atomically.
func TestMintAllowlist_ApprovedAddsRowsTokenAndAudits(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	tok, ident := mustBootstrap(t, s)
	ctx := context.Background()
	conn := mustFrontDoorConn(t, s, ident.UserID())

	want := []string{"198.51.100.0/24", "203.0.113.7/32"}
	out, err := s.CreatePAT(ctx, tok, "library", conn, 0, want, false, want, testIP)
	if err != nil {
		t.Fatalf("CreatePAT: %v", err)
	}
	if out.Secret == "" {
		t.Fatal("no secret returned")
	}
	rows := rowsOf(t, s, ident.UserID())
	for _, c := range want {
		label, ok := rows[c]
		if !ok {
			t.Errorf("%s was approved and not added", c)
			continue
		}
		// PROVENANCE: the immutable id AND a snapshotted name, because names
		// are reusable and the token may be gone when somebody reads this row
		// and decides whether to remove it.
		if !strings.Contains(label, "#") || !strings.Contains(label, "library") {
			t.Errorf("%s label %q carries neither the token id nor its name", c, label)
		}
		// And never the credential itself.
		if strings.Contains(label, out.Secret) {
			t.Errorf("%s label carries the SECRET", c)
		}
	}
	if n := patAuditCount(t, s, "user_ip_added"); n != len(want) {
		t.Errorf("audit rows = %d, want one per added row (%d)", n, len(want))
	}
	if n := patAuditCount(t, s, "pat_created"); n != 1 {
		t.Errorf("pat_created audit rows = %d, want 1", n)
	}
}

// AN ALREADY-COVERED ADDRESS ADDS NOTHING, relabels nothing, audits nothing.
//
// Contained, not equal: this is what stops a repeated mint from growing the
// allowlist one row at a time.
func TestMintAllowlist_ContainedAndDuplicateRequestsChangeNothing(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	tok, ident := mustBootstrap(t, s)
	ctx := context.Background()
	conn := mustFrontDoorConn(t, s, ident.UserID())
	seedRow(t, s, ident.UserID(), "10.0.0.0/8", "office")

	// 10.1.2.0/24 is inside 10.0.0.0/8, and the duplicate must not double
	// anything either.
	if _, err := s.CreatePAT(ctx, tok, "inside", conn, 0,
		[]string{"10.1.2.0/24", "10.1.2.0/24"}, false, nil, testIP); err != nil {
		t.Fatalf("a contained address was refused: %v", err)
	}
	rows := rowsOf(t, s, ident.UserID())
	if len(rows) != 1 {
		t.Errorf("rows = %v, want only the seeded one", rows)
	}
	if rows["10.0.0.0/8"] != "office" {
		t.Errorf("the existing row was RELABELLED: %q", rows["10.0.0.0/8"])
	}
	if n := patAuditCount(t, s, "user_ip_added"); n != 0 {
		t.Errorf("an already-covered CIDR wrote %d audit rows, claiming a change that did "+
			"not happen", n)
	}
}

// A WIDER APPROVED REQUEST CREATES ITS OWN ROW AND LEAVES THE NARROW ONE.
//
// Never merge, widen, relabel or delete an existing overlapping row: the
// operator's other rows are not this operation's to reorganise.
func TestMintAllowlist_OverlapPreservesEveryExistingRow(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	tok, ident := mustBootstrap(t, s)
	ctx := context.Background()
	conn := mustFrontDoorConn(t, s, ident.UserID())
	seedRow(t, s, ident.UserID(), "192.168.1.0/24", "home")

	wider := []string{"192.168.0.0/16"}
	if _, err := s.CreatePAT(ctx, tok, "wider", conn, 0, wider, false, wider, testIP); err != nil {
		t.Fatalf("CreatePAT: %v", err)
	}
	rows := rowsOf(t, s, ident.UserID())
	if _, ok := rows["192.168.1.0/24"]; !ok {
		t.Error("the narrower pre-existing row was removed or merged away")
	}
	if rows["192.168.1.0/24"] != "home" {
		t.Errorf("the pre-existing row was relabelled: %q", rows["192.168.1.0/24"])
	}
	if _, ok := rows["192.168.0.0/16"]; !ok {
		t.Error("the approved wider row was not created")
	}
}

// A STALE APPROVAL REFUSES AND COMMITS NOTHING, and carries the new set.
//
// The request and the approval are different sets. If a requested CIDR was
// already covered when the prompt was drawn, the prompt showed the others --
// and if its covering row is removed in the meantime, "in the request" would
// silently admit a CIDR the operator was never shown.
func TestMintAllowlist_ExpandedMissingSetIsRefusedWithTheNewSet(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	tok, ident := mustBootstrap(t, s)
	ctx := context.Background()
	conn := mustFrontDoorConn(t, s, ident.UserID())

	// The prompt was drawn while 10.0.0.0/8 covered A, so it approved only B.
	// That covering row is now gone.
	approved := []string{"203.0.113.7/32"}
	requested := []string{"10.1.2.0/24", "203.0.113.7/32"}

	_, err := s.CreatePAT(ctx, tok, "stale", conn, 0, requested, false, approved, testIP)
	var stale *StaleApproval
	if !errors.As(err, &stale) {
		t.Fatalf("an expanded missing set was not refused as stale: %v", err)
	}
	if !errors.Is(err, ErrApprovalStale) {
		t.Error("the stale outcome is not matchable with errors.Is, so a caller cannot tell " +
			"it from a fault and will report a failure instead of re-prompting")
	}
	if len(stale.Missing) != 2 {
		t.Errorf("the refusal does not carry the new exact set: %v", stale.Missing)
	}
	if got := rowsOf(t, s, ident.UserID()); len(got) != 0 {
		t.Errorf("the refused mint added rows: %v", got)
	}
	if n := patAuditCount(t, s, "pat_created"); n != 0 {
		t.Error("the refused mint created a token")
	}
}

// AND A SHRUNK SET SUCCEEDS, adding only what is still missing.
//
// The positive control for the cell above: without it, "refuse whenever the
// set changed" would be a defensible reading of the rule, and it would break
// the ordinary race that costs nothing.
func TestMintAllowlist_ShrunkMissingSetAddsOnlyTheRemainder(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	tok, ident := mustBootstrap(t, s)
	ctx := context.Background()
	conn := mustFrontDoorConn(t, s, ident.UserID())

	// Approved two; by mint time one is already covered, so only the other is
	// still missing. Adding FEWER rows than approved needs no new consent.
	seedRow(t, s, ident.UserID(), "198.51.100.0/24", "already here")
	approved := []string{"198.51.100.5/32", "203.0.113.7/32"}
	requested := approved

	if _, err := s.CreatePAT(ctx, tok, "shrunk", conn, 0, requested, false, approved, testIP); err != nil {
		t.Fatalf("a shrunk missing set was refused: %v", err)
	}
	rows := rowsOf(t, s, ident.UserID())
	if _, ok := rows["203.0.113.7/32"]; !ok {
		t.Error("the still-missing approved CIDR was not added")
	}
	if _, ok := rows["198.51.100.5/32"]; ok {
		t.Error("a CIDR already covered by an existing row was added anyway")
	}
	if n := patAuditCount(t, s, "user_ip_added"); n != 1 {
		t.Errorf("audit rows = %d, want exactly one for the single row added", n)
	}
}

// A CLEARTEXT-DEBUG TOKEN NEVER WIDENS THE USER ALLOWLIST.
//
// Its own non-empty narrow list is the ENTIRE gate for that class, so adding
// user rows is unnecessary for the token AND would manufacture standing
// admission for later TLS and password use -- exposure in a class it was never
// granted.
func TestMintAllowlist_DebugCleartextTokenNeverWidens(t *testing.T) {
	t.Parallel()
	// SERVING CLEARTEXT, deliberately: on an ordinary daemon the mint is
	// refused earlier for not serving cleartext at all, and this cell is
	// about the WIDENING rule, not that one. A cell that passes because a
	// different refusal fired first proves nothing about its own subject.
	s, _, _ := cleartextSvc(t, true)
	tok, ident := mustBootstrap(t, s)
	ctx := context.Background()
	conn := mustFrontDoorConn(t, s, ident.UserID())

	approved := []string{"203.0.113.7/32"}
	_, err := s.CreatePAT(ctx, tok, "debug", conn, 0, approved, true, approved, testIP)
	if !errors.Is(err, ErrPATDebugNoWiden) {
		t.Fatalf("a debug token was allowed to carry an allowlist approval: %v", err)
	}
	if got := rowsOf(t, s, ident.UserID()); len(got) != 0 {
		t.Errorf("a debug mint added user rows: %v", got)
	}
}

// THE CAP IS ENFORCED AGAINST THE WHOLE ADDITION, and refusing rolls back the
// token too: a token whose restriction its owner's admission cannot satisfy is
// a credential that cannot be used.
func TestMintAllowlist_CapRefusalRollsBackTheWholeMint(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	tok, ident := mustBootstrap(t, s)
	ctx := context.Background()
	conn := mustFrontDoorConn(t, s, ident.UserID())

	// Fill to one below the cap, then ask for two.
	for i := 0; i < maxUserIPs-1; i++ {
		seedRow(t, s, ident.UserID(), cidrN(i), "filler")
	}
	want := []string{"203.0.113.1/32", "203.0.113.2/32"}
	out, err := s.CreatePAT(ctx, tok, "over-cap", conn, 0, want, false, want, testIP)
	if err == nil {
		t.Fatal("two rows were added over the cap")
	}
	if out.Secret != "" {
		t.Error("a refused mint returned a secret")
	}
	rows := rowsOf(t, s, ident.UserID())
	if len(rows) != maxUserIPs-1 {
		t.Errorf("rows = %d, want the %d seeded ones untouched", len(rows), maxUserIPs-1)
	}
	if n := patAuditCount(t, s, "pat_created"); n != 0 {
		t.Error("the rolled-back mint left a pat_created audit row")
	}
}

// cidrN yields distinct /32s for filling the cap.
func cidrN(i int) string {
	return "10." + itoa(i/256) + "." + itoa(i%256) + ".1/32"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// A DISABLED OWNER KEEPS THE ROWS AND LOSES THE ADMISSION, and re-enabling
// restores only what was retained. Token expiry and revocation never touch
// them: the row is the user's, and only the OCCASION of its creation belonged
// to a token.
func TestMintAllowlist_OwnerLifecycleAndTokenLifecycleAreSeparate(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	rootTok, root := mustBootstrap(t, s)
	ctx := context.Background()

	devID, err := s.CreateUser(ctx, rootTok, "dev", "dev-passphrase-long", meta.RoleEditor, testIP)
	if err != nil {
		t.Fatal(err)
	}
	devTok, _, err := s.Login(ctx, "dev", "dev-passphrase-long", testIP)
	if err != nil {
		t.Fatal(err)
	}
	conn := mustFrontDoorConn(t, s, root.UserID())
	if err := s.AddGrant(ctx, rootTok, devID, conn, meta.RoleReader, testIP); err != nil {
		t.Fatal(err)
	}

	want := []string{"203.0.113.9/32"}
	if _, err := s.CreatePAT(ctx, devTok, "libtoken", conn, 0, want, false, want, testIP); err != nil {
		t.Fatalf("CreatePAT: %v", err)
	}
	if _, ok := rowsOf(t, s, devID)["203.0.113.9/32"]; !ok {
		t.Fatal("the approved row was not added; the rest of this cell would prove nothing")
	}

	// Revoking the token leaves the row.
	if err := s.RevokePAT(ctx, devTok, devID, "libtoken"); err != nil {
		t.Fatal(err)
	}
	if _, ok := rowsOf(t, s, devID)["203.0.113.9/32"]; !ok {
		t.Error("revoking the token deleted the owner's row -- the token never owned it")
	}

	// Disabling the owner keeps the row and makes admission inert.
	if err := s.SetUserDisabled(ctx, rootTok, devID, true, testIP); err != nil {
		t.Fatal(err)
	}
	if _, ok := rowsOf(t, s, devID)["203.0.113.9/32"]; !ok {
		t.Error("disabling the owner deleted the row; disable is inert, not destructive")
	}
	// THE SECOND LAYER, which is what these rows feed. Login's own `ip`
	// argument is judged against the GLOBAL allowlist; the per-user rows are
	// the admission layer LoginAt takes separately -- so asserting through
	// Login would have tested the global list and called it a per-user row.
	if _, _, err := s.LoginAt(ctx, "dev", "dev-passphrase-long", testIP, "203.0.113.9"); err == nil {
		t.Error("a disabled owner was admitted from a retained row")
	}

	// Re-enabling reactivates exactly what was retained.
	if err := s.SetUserDisabled(ctx, rootTok, devID, false, testIP); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LoginAt(ctx, "dev", "dev-passphrase-long", testIP, "203.0.113.9"); err != nil {
		t.Errorf("re-enabling did not restore the retained row's admission: %v", err)
	}
	// AND THE POSITIVE CONTROL for the layer itself: an address with no row
	// is still refused, so the success above is the ROW admitting and not the
	// layer being open.
	if _, _, err := s.LoginAt(ctx, "dev", "dev-passphrase-long", testIP, "198.51.100.77"); err == nil {
		t.Error("an address with no row was admitted; the per-user layer is not gating at all")
	}
}

// A DISABLE THAT LANDS BEFORE THE LOCK MUST STOP THE MINT ENTIRELY.
//
// CreatePAT resolves the session before its transaction opens. That was enough
// when the only write was a token; this path now creates DURABLE ADMISSION, so
// an admin disabling the owner in that window would otherwise leave a token
// plus standing rows for a disabled account -- rows that become effective the
// moment it is re-enabled. Dormant standing admission is exactly what nobody
// audits.
//
// Driven through a hook rather than a goroutine: the window is too short for a
// race to hit reliably, and a cell that only sometimes observes the defect
// reports the fix as working.
func TestMintAllowlist_DisableBeforeTheLockCommitsNothing(t *testing.T) {
	s, _, _ := newSvc(t)
	rootTok, root := mustBootstrap(t, s)
	ctx := context.Background()

	devID, err := s.CreateUser(ctx, rootTok, "dev", "dev-passphrase-long", meta.RoleEditor, testIP)
	if err != nil {
		t.Fatal(err)
	}
	devTok, _, err := s.Login(ctx, "dev", "dev-passphrase-long", testIP)
	if err != nil {
		t.Fatal(err)
	}
	conn := mustFrontDoorConn(t, s, root.UserID())
	if err := s.AddGrant(ctx, rootTok, devID, conn, meta.RoleReader, testIP); err != nil {
		t.Fatal(err)
	}

	// The disable lands in the window between the outer validation and the
	// lock.
	s.hookBeforeMintTx = func() {
		if derr := s.SetUserDisabled(ctx, rootTok, devID, true, testIP); derr != nil {
			t.Fatalf("disabling in the window: %v", derr)
		}
	}
	want := []string{"203.0.113.9/32"}
	out, err := s.CreatePAT(ctx, devTok, "racer", conn, 0, want, false, want, testIP)
	s.hookBeforeMintTx = nil

	if err == nil {
		t.Error("a mint for an owner disabled before the lock succeeded")
	}
	if out.Secret != "" {
		t.Error("the refused mint returned a secret")
	}
	if got := rowsOf(t, s, devID); len(got) != 0 {
		t.Errorf("the refused mint left STANDING allowlist rows for a disabled account: %v", got)
	}
	if n := patAuditCount(t, s, "pat_created"); n != 0 {
		t.Error("the refused mint left a pat_created audit row")
	}
}

// HARD DELETION CASCADES the rows, because they are the user's and the user is
// gone. Asserted so the lifecycle is stated in full: expiry no, revoke no,
// disable no, delete yes.
func TestMintAllowlist_HardUserDeletionCascadesTheRows(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	rootTok, root := mustBootstrap(t, s)
	ctx := context.Background()

	devID, err := s.CreateUser(ctx, rootTok, "dev", "dev-passphrase-long", meta.RoleEditor, testIP)
	if err != nil {
		t.Fatal(err)
	}
	devTok, _, err := s.Login(ctx, "dev", "dev-passphrase-long", testIP)
	if err != nil {
		t.Fatal(err)
	}
	conn := mustFrontDoorConn(t, s, root.UserID())
	if err := s.AddGrant(ctx, rootTok, devID, conn, meta.RoleReader, testIP); err != nil {
		t.Fatal(err)
	}
	want := []string{"203.0.113.11/32"}
	if _, err := s.CreatePAT(ctx, devTok, "doomed", conn, 0, want, false, want, testIP); err != nil {
		t.Fatal(err)
	}
	if len(rowsOf(t, s, devID)) != 1 {
		t.Fatal("the row was not added; the cascade below would prove nothing")
	}

	if err := s.RemoveUser(ctx, rootTok, devID, testIP); err != nil {
		t.Fatalf("RemoveUser: %v", err)
	}
	if got := rowsOf(t, s, devID); len(got) != 0 {
		t.Errorf("hard deletion left the account's allowlist rows behind: %v", got)
	}
}

// NO CREDENTIAL MATERIAL IN THE AUDIT TRAIL OR THE ROW LABELS.
//
// The trail and the labels are read by people and shipped to logs; a secret or
// a selector in either is a credential in a place nobody protects.
func TestMintAllowlist_AuditAndLabelsCarryNoCredentialMaterial(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	tok, ident := mustBootstrap(t, s)
	ctx := context.Background()
	conn := mustFrontDoorConn(t, s, ident.UserID())

	want := []string{"203.0.113.13/32"}
	out, err := s.CreatePAT(ctx, tok, "hygiene", conn, 0, want, false, want, testIP)
	if err != nil {
		t.Fatal(err)
	}
	// The selector is the public half; it identifies the token to the wire and
	// still does not belong in a label or an audit detail.
	selector := out.Secret
	if i := strings.IndexByte(out.Secret, '.'); i > 0 {
		selector = out.Secret[:i]
	}

	rows, err := s.store.Audit.OnCtx(ctx).Select()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("no audit rows at all; this cell would prove nothing")
	}
	for _, r := range rows {
		if strings.Contains(r.Detail, out.Secret) {
			t.Errorf("audit %q carries the SECRET", r.Action)
		}
		if strings.Contains(r.Detail, selector) {
			t.Errorf("audit %q carries the selector", r.Action)
		}
	}
	for cidr, label := range rowsOf(t, s, ident.UserID()) {
		if strings.Contains(label, out.Secret) || strings.Contains(label, selector) {
			t.Errorf("row %s label carries credential material: %q", cidr, label)
		}
	}
}

// A DEMOTION BEFORE THE LOCK MUST REACH THE DEBUG-CLASS CHECK.
//
// The first version of the in-transaction authority check resolved the fresh
// identity and then DISCARDED it, continuing with the pre-lock role. That left
// a deterministic hole a review found: demote an admin in the window, and the
// debug-cleartext branch still saw a stale admin, so it could mint an
// ADMIN-ONLY credential for an account that no longer had the role.
//
// Re-checking and not using the answer is worse than not checking, because it
// reads as a guard.
func TestMintAllowlist_DemotionBeforeTheLockDeniesTheDebugClass(t *testing.T) {
	s, _, _ := cleartextSvc(t, true) // serving cleartext, so the class is reachable
	rootTok, root := mustBootstrap(t, s)
	ctx := context.Background()

	// A second admin, so demoting them does not trip the last-admin guard.
	otherID, err := s.CreateUser(ctx, rootTok, "other", "other-passphrase-long", meta.RoleAdmin, testIP)
	if err != nil {
		t.Fatal(err)
	}
	otherTok, _, err := s.Login(ctx, "other", "other-passphrase-long", testIP)
	if err != nil {
		t.Fatal(err)
	}
	conn := mustFrontDoorConn(t, s, root.UserID())
	if err := s.AddGrant(ctx, rootTok, otherID, conn, meta.RoleReader, testIP); err != nil {
		t.Fatal(err)
	}

	// POSITIVE CONTROL: while still an admin, this mint SUCCEEDS -- so the
	// refusal below is caused by the demotion and by nothing else.
	if _, err := s.CreatePAT(ctx, otherTok, "debug-ok", conn, 0,
		[]string{"203.0.113.20/32"}, true, nil, testIP); err != nil {
		t.Fatalf("an admin on a cleartext daemon could not mint a debug token: %v", err)
	}

	// Now demote inside the window between the outer validation and the lock.
	s.hookBeforeMintTx = func() {
		if derr := s.SetUserRole(ctx, rootTok, otherID, meta.RoleEditor, testIP); derr != nil {
			t.Fatalf("demoting in the window: %v", derr)
		}
	}
	out, err := s.CreatePAT(ctx, otherTok, "debug-stale", conn, 0,
		[]string{"203.0.113.21/32"}, true, nil, testIP)
	s.hookBeforeMintTx = nil

	if err == nil {
		t.Error("a demoted admin minted an admin-only cleartext credential using its " +
			"pre-lock role")
	}
	if out.Secret != "" {
		t.Error("the refused mint returned a secret")
	}
}

// A BARE ADDRESS IS ACCEPTED, because the form accepts and forwards one.
//
// The first version used net.ParseCIDR alone, which rejects a bare address --
// so the exact input this feature was asked for, 203.0.113.7, failed at
// preview. Reusing the established canonicalCIDR gives both forms and the
// v4/v6/4-in-6 parity a second parser would have to re-derive.
func TestMintAllowlist_BareAddressesAreCanonicalized(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	tok, ident := mustBootstrap(t, s)
	ctx := context.Background()

	// Preview first: this is the call the form makes, and where it failed.
	missing, err := s.PATAllowlistAdditions(ctx, tok, []string{"203.0.113.7"})
	if err != nil {
		t.Fatalf("a bare address was rejected at preview: %v", err)
	}
	if len(missing) != 1 || missing[0] != "203.0.113.7/32" {
		t.Fatalf("preview = %v, want the canonical /32", missing)
	}

	// And the mint accepts the same bare form, approved by its canonical one.
	conn := mustFrontDoorConn(t, s, ident.UserID())
	if _, err := s.CreatePAT(ctx, tok, "bare", conn, 0,
		[]string{"203.0.113.7"}, false, []string{"203.0.113.7/32"}, testIP); err != nil {
		t.Fatalf("a bare address was rejected at mint: %v", err)
	}
	if _, ok := rowsOf(t, s, ident.UserID())["203.0.113.7/32"]; !ok {
		t.Error("the bare address did not become a canonical /32 row")
	}

	// A v6 bare address too, since the parity is the reason for reusing the
	// established parser rather than writing one here.
	m6, err := s.PATAllowlistAdditions(ctx, tok, []string{"2001:db8::1"})
	if err != nil {
		t.Fatalf("a bare v6 address was rejected: %v", err)
	}
	if len(m6) != 1 || m6[0] != "2001:db8::1/128" {
		t.Errorf("v6 preview = %v, want the canonical /128", m6)
	}
}

// THE AUDIT TRAIL RECORDS WHERE FROM, not just who.
//
// Both audit rows this path writes used to carry an empty address: the trail
// said who minted a token and never from where, and the allowlist rows it now
// creates are exactly the change an investigation wants an address for.
func TestMintAllowlist_AuditsRecordThePeerAddress(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	tok, ident := mustBootstrap(t, s)
	ctx := context.Background()
	conn := mustFrontDoorConn(t, s, ident.UserID())

	const peer = "198.51.100.42"
	want := []string{"203.0.113.31/32"}
	if _, err := s.CreatePAT(ctx, tok, "traced", conn, 0, want, false, want, peer); err != nil {
		t.Fatal(err)
	}

	rows, err := s.store.Audit.OnCtx(ctx).Select()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, r := range rows {
		if r.Action == "pat_created" || r.Action == "user_ip_added" {
			seen[r.Action] = r.IP
		}
	}
	for _, action := range []string{"pat_created", "user_ip_added"} {
		got, ok := seen[action]
		if !ok {
			t.Errorf("no %s audit row at all", action)
			continue
		}
		if got != peer {
			t.Errorf("%s recorded IP %q, want the caller's %q", action, got, peer)
		}
	}
}

// A FAILING AUDIT ROLLS THE WHOLE OPERATION BACK.
//
// The audit rides the same transaction as the token and the rows on purpose:
// a token that exists with no record of its creation is exactly the token an
// investigation cannot account for, and an allowlist row with no record of who
// widened admission is worse. So the audit failing must take everything with
// it -- and a rollback nobody has observed is a rollback nobody should
// believe, which is why this drives a seam rather than asserting the comment.
//
// THE SEAM IS IN THE AUDIT WRITE, not at the call site, and the difference is
// the whole value of the cell. A review caught the first version injecting
// immediately BEFORE AuditTx in addMintAllowlistRows: with the failure upstream
// of the call, deleting the AuditTx line -- or writing `_ = s.AuditTx(...)` --
// left this cell green, because the injected error fired either way and the
// mint failed for that reason while the audit had silently stopped happening.
// Firing from inside the persisted write makes both of those RED.
func TestMintAllowlist_AFailingAuditRollsBackTokenAndRows(t *testing.T) {
	s, _, _ := newSvc(t)
	tok, ident := mustBootstrap(t, s)
	ctx := context.Background()
	conn := mustFrontDoorConn(t, s, ident.UserID())

	// POSITIVE CONTROL: the same mint succeeds with the seam inert, so the
	// failure below is the injected one and not something else.
	ok := []string{"203.0.113.41/32"}
	if _, err := s.CreatePAT(ctx, tok, "control", conn, 0, ok, false, ok, testIP); err != nil {
		t.Fatalf("the control mint failed: %v", err)
	}
	// And the control really wrote the audit the failing cell is about, so a
	// mint that stopped auditing rows entirely cannot reach the assertions
	// below and call itself proven.
	if n := patAuditCount(t, s, "user_ip_added"); n != 1 {
		t.Fatalf("user_ip_added audits = %d after one widening mint, want 1: without that "+
			"row this cell says nothing about the audit", n)
	}
	before := rowsOf(t, s, ident.UserID())
	beforeAudits := patAuditCount(t, s, "pat_created")

	s.hookAuditWrite = failAudit("user_ip_added")
	want := []string{"203.0.113.42/32"}
	out, err := s.CreatePAT(ctx, tok, "doomed", conn, 0, want, false, want, testIP)
	s.hookAuditWrite = nil

	if err == nil {
		t.Fatal("a failing audit did not fail the mint")
	}
	if out.Secret != "" {
		t.Error("the rolled-back mint returned a secret")
	}
	if got := rowsOf(t, s, ident.UserID()); len(got) != len(before) {
		t.Errorf("the rolled-back mint left allowlist rows: %v (was %v)", got, before)
	}
	if _, ok := rowsOf(t, s, ident.UserID())["203.0.113.42/32"]; ok {
		t.Error("the row survived a rolled-back mint")
	}
	if n := patAuditCount(t, s, "pat_created"); n != beforeAudits {
		t.Errorf("pat_created audits = %d, want the control's %d: the token survived",
			n, beforeAudits)
	}
	// And the token itself is gone, not merely unaudited.
	rows, lerr := s.ListPATs(ctx, tok, ident.UserID())
	if lerr != nil {
		t.Fatal(lerr)
	}
	for _, r := range rows {
		if r.Name == "doomed" {
			t.Error("the token survived a rolled-back mint")
		}
	}
}

var errAuditProbe = errors.New("auth: injected audit failure (test)")

// failAudit fails exactly one action's audit write and passes every other
// through.
//
// One action, because a mint writes several audits in one transaction and a
// seam that failed all of them could not tell which call site was exercised --
// the row audit is the one under test here.
func failAudit(action string) func(string) error {
	return func(got string) error {
		if got == action {
			return errAuditProbe
		}
		return nil
	}
}

// THE SAME BEHAVIOUR ON POSTGRESQL, because that is where the transaction
// semantics differ.
//
// sqlite serializes writers, which HIDES the difference between "atomic write"
// and "exclusive decision" -- the distinction that already produced two real
// defects in this package (19 tokens against a cap of 16, and 35 allowlist
// rows against a cap of 32), both reproducible only on PostgreSQL. A combined
// mint that inserts a token AND admission rows is exactly the shape that
// warrants checking there rather than assuming sqlite's answer generalises.
func TestMintAllowlist_PostgresRollbackAndContainment(t *testing.T) {
	ctx := context.Background()
	// ITS OWN SCHEMA. On the shared one this cell skipped whenever another PG
	// cell had bootstrapped first -- which is every run after the database is
	// created. See pg_isolated_test.go.
	store := pgIsolatedStore(t, "widen")
	s, err := New(store, WithConfigAllowlist([]string{"127.0.0.1/32", "::1/128"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	name := fmt.Sprintf("widen%d", time.Now().UnixNano())
	tok, ident := mustBootstrapPG(t, s, name)
	conn := mustFrontDoorConn(t, s, ident.UserID())

	// 1. APPROVED adds the row and the token together.
	//
	// The audit count is taken as a DELTA, not an absolute: this store is
	// shared and long-lived, so any absolute count is a count of every run
	// before this one.
	auditsBefore := patAuditCount(t, s, "user_ip_added")
	want := []string{"203.0.113.51/32"}
	if _, cerr := s.CreatePAT(ctx, tok, name+"-a", conn, 0, want, false, want, testIP); cerr != nil {
		t.Fatalf("CreatePAT: %v", cerr)
	}
	if got := patAuditCount(t, s, "user_ip_added") - auditsBefore; got != 1 {
		t.Fatalf("user_ip_added audits grew by %d on postgres, want 1: step 2 injects into "+
			"that write, so without it this cell proves nothing there either", got)
	}
	rows, rerr := s.UserIPs(ctx, tok, ident.UserID())
	if rerr != nil {
		t.Fatal(rerr)
	}
	found := false
	for _, r := range rows {
		if r.CIDR == "203.0.113.51/32" {
			found = true
		}
	}
	if !found {
		t.Fatal("the approved row was not added on postgres")
	}
	rowsBefore := len(rows)

	// 2. A FAILING AUDIT rolls the whole thing back -- the case where
	// PostgreSQL's rollback, rather than sqlite's serialized writer, is doing
	// the work.
	s.hookAuditWrite = failAudit("user_ip_added")
	out, cerr := s.CreatePAT(ctx, tok, name+"-b", conn, 0,
		[]string{"203.0.113.52/32"}, false, []string{"203.0.113.52/32"}, testIP)
	s.hookAuditWrite = nil
	if cerr == nil {
		t.Fatal("the injected audit failure did not fail the mint")
	}
	if out.Secret != "" {
		t.Error("the rolled-back mint returned a secret")
	}
	rows, rerr = s.UserIPs(ctx, tok, ident.UserID())
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(rows) != rowsBefore {
		t.Errorf("rows = %d after a rolled-back mint, want the previous %d", len(rows), rowsBefore)
	}

	// 3. CONTAINMENT holds: a second mint inside the row it just created adds
	// nothing and needs no approval.
	if _, cerr := s.CreatePAT(ctx, tok, name+"-c", conn, 0,
		[]string{"203.0.113.51/32"}, false, nil, testIP); cerr != nil {
		t.Fatalf("a contained address was refused on postgres: %v", cerr)
	}
	rows, rerr = s.UserIPs(ctx, tok, ident.UserID())
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(rows) != rowsBefore {
		t.Errorf("a contained address added a row on postgres: %d, want %d", len(rows), rowsBefore)
	}
}
