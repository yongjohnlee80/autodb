package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// LoginAt's combined operation (ADR-0075 Amendment 1, lector PR #34 r2
// ruling).
//
// The gateway used to log in, ask a second RPC whether the browser's address
// was admitted, and log the session out again when it was not — strictly more
// work for a CORRECT password than an incorrect one, and measurable. The
// decision lives here now, between verifying the credential and minting
// anything.

// EVERY REFUSAL PATH ISSUES EXACTLY ONE ADMISSION LOOKUP.
//
// This is the decoy's real contract, and it is asserted structurally because
// it cannot be asserted temporally where it matters. Against this in-memory
// store one extra read is invisible next to an HTTP round trip, so the
// gateway's timing cells cannot see the decoy missing — I checked, by
// removing it, and they stayed green. Against a Postgres meta store the same
// query is a network hop and the difference is exactly the kind these
// defences exist for. Counting the queries observes the property directly and
// does not depend on how fast the store happens to be.
func TestLoginAt_EveryRefusalPathCostsOneAdmissionLookup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, _ := newSvc(t)
	if _, _, err := s.Bootstrap(ctx, "johno", rootPass, testIP); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	// An address the global list does not carry and the user has no row for.
	const refused = "203.0.113.9"

	for _, c := range []struct{ name, user, pass string }{
		{"an unknown name", "nobody-at-all", rootPass},
		{"a real name with a wrong password", "johno", "not the passphrase"},
		{"a real name with the RIGHT password", "johno", rootPass},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := s.admissionQueries.Load()
			_, _, err := s.LoginAt(ctx, c.user, c.pass, testIP, refused)
			if !errors.Is(err, ErrBadCredentials) {
				t.Fatalf("err = %v, want the uniform ErrBadCredentials", err)
			}
			if got := s.admissionQueries.Load() - before; got != 1 {
				t.Errorf("%d admission lookups, want exactly 1. A path that skips it does less "+
					"work than the others, and against a meta store that is a network hop "+
					"away that difference is what tells a caller which path they took", got)
			}
		})
	}
}

// A caller who will be refused gets NO SESSION.
//
// Minting one and revoking it is precisely the extra work that let a correct
// password be told from an incorrect one by timing. The absence of the row is
// the durable evidence that it is not happening.
func TestLoginAt_ARefusedAddressMintsNoSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, store, _ := newSvc(t)
	if _, _, err := s.Bootstrap(ctx, "johno", rootPass, testIP); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	before, err := store.Sessions.OnCtx(ctx).Count()
	if err != nil {
		t.Fatal(err)
	}

	tok, _, lerr := s.LoginAt(ctx, "johno", rootPass, testIP, "203.0.113.9")
	if !errors.Is(lerr, ErrBadCredentials) {
		t.Fatalf("err = %v, want the uniform ErrBadCredentials", lerr)
	}
	if tok != "" {
		t.Error("a token was issued to a caller who was refused")
	}
	after, err := store.Sessions.OnCtx(ctx).Count()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("%d session rows were created for a refused login; minting and revoking one is "+
			"the extra work that made a correct password measurably slower than a wrong one",
			after-before)
	}
}

// The refusal is UNIFORM: an address neither layer admits gets the same error
// a wrong password gets, and the audit says which it was.
func TestLoginAt_TheRefusalIsUniformAndTheAuditIsNot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, store, _ := newSvc(t)
	if _, _, err := s.Bootstrap(ctx, "johno", rootPass, testIP); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	_, _, wrongErr := s.LoginAt(ctx, "johno", "not the passphrase", testIP, testIP)
	_, _, addrErr := s.LoginAt(ctx, "johno", rootPass, testIP, "203.0.113.9")
	if wrongErr.Error() != addrErr.Error() {
		t.Errorf("a wrong password returned %q and a refused address %q; the two must be "+
			"indistinguishable to the caller", wrongErr, addrErr)
	}

	rows, err := store.Audit.OnCtx(ctx).With(meta.AuditAction, "login_failed").Select()
	if err != nil {
		t.Fatal(err)
	}
	var sawAddress bool
	for _, r := range rows {
		if r.Detail == "johno (ip not admitted)" {
			sawAddress = true
		}
	}
	if !sawAddress {
		t.Error("the audit trail does not distinguish the refused address; the wire is uniform " +
			"precisely so the trail can be specific, and an operator has nothing to read")
	}
}

// An ADMITTED address still logs in, or the check has simply banned everyone.
func TestLoginAt_AnAdmittedAddressStillLogsIn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, _ := newSvc(t)
	if _, _, err := s.Bootstrap(ctx, "johno", rootPass, testIP); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	tok, ident, err := s.LoginAt(ctx, "johno", rootPass, testIP, testIP)
	if err != nil {
		t.Fatalf("an admitted address was refused: %v", err)
	}
	if tok == "" || ident.Name() != "johno" {
		t.Errorf("token %q ident %+v", tok, ident)
	}
}

// Login is LoginAt with no second layer, unchanged for every existing caller.
func TestLogin_IsLoginAtWithoutAnAdmissionLayer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, _ := newSvc(t)
	if _, _, err := s.Bootstrap(ctx, "johno", rootPass, testIP); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	// BOTH the success and the FAILURE paths, because the decoy only runs on
	// failures — a cell that logged in successfully could not see an
	// unconditional decoy at all, and the first version of this one could
	// not: the mutation removing the decoy's own guard survived it.
	before := s.admissionQueries.Load()
	if _, _, err := s.Login(ctx, "johno", rootPass, testIP); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if _, _, err := s.Login(ctx, "johno", "not the passphrase", testIP); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("a wrong password returned %v", err)
	}
	if _, _, err := s.Login(ctx, "nobody-at-all", rootPass, testIP); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("an unknown name returned %v", err)
	}
	if got := s.admissionQueries.Load() - before; got != 0 {
		t.Errorf("Login issued %d admission lookups across a success, a wrong password and an "+
			"unknown name; a caller with no browser address must not acquire a second layer it "+
			"never asked for, and the decoy has to MIRROR the real path rather than do "+
			"unconditional work", got)
	}
}

// THE ADMISSION RECORD COMMITS WITH THE SESSION OR NOT AT ALL (lector PR #34
// r3 must-fix).
//
// It used to be written the moment the admission check passed, under a
// comment claiming the login had already been decided. It had not. Between
// that point and the commit lie a keyslot check, a master-key unwrap, and a
// transactional recheck that exists precisely because a disable or a
// passphrase reset can land underneath an in-flight login. Each of those left
// a durable row asserting an admitted login that never existed — an audit
// trail that lies is worse than one that is silent, because it is the thing
// an operator reconstructs an incident from.
//
// Reproduced the way lector did: deterministically, with an empty keyslot,
// rather than by arguing about a race.
func TestLoginAt_NoAdmissionRecordWithoutACommittedSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, store, _ := newSvc(t)
	_, ident, err := s.Bootstrap(ctx, "johno", rootPass, testIP)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// A row that never received a keyslot: correct credentials, admitted
	// address, and a login that still cannot complete.
	if uerr := store.Users.OnCtx(ctx).With(meta.UserID, ident.UserID()).
		Set(meta.UserMKWrapped, []byte{}).Update(); uerr != nil {
		t.Fatalf("clearing the keyslot: %v", uerr)
	}

	before := countAuditRows(t, store, "login_admitted")
	sessBefore := countSessions(t, store)

	tok, _, lerr := s.LoginAt(ctx, "johno", rootPass, testIP, testIP)
	if !errors.Is(lerr, ErrNoKeyslot) {
		t.Fatalf("err = %v, want ErrNoKeyslot — this cell needs a login that fails AFTER the "+
			"admission check and before the commit, or it observes nothing", lerr)
	}
	if tok != "" {
		t.Error("a token was issued despite the failure")
	}
	if n := countSessions(t, store) - sessBefore; n != 0 {
		t.Fatalf("%d sessions were created by a failed login", n)
	}
	if n := countAuditRows(t, store, "login_admitted") - before; n != 0 {
		t.Errorf("%d admission-success rows were written for a login that never completed. The "+
			"trail now asserts an admitted session that does not exist, which is exactly what "+
			"someone reconstructing an access would read", n)
	}
}

// The positive control: when the transaction DOES commit, the source is
// recorded. Without this, the cell above is satisfied by never writing the
// row at all, and an operator loses the distinction between a login from
// shared infrastructure and one from a person's own registered address.
func TestLoginAt_AnAdmittedLoginRecordsWhichLayerAdmittedIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, store, _ := newSvc(t)
	if _, _, err := s.Bootstrap(ctx, "johno", rootPass, testIP); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	if _, _, err := s.LoginAt(ctx, "johno", rootPass, testIP, testIP); err != nil {
		t.Fatalf("an admitted login failed: %v", err)
	}
	rows, err := store.Audit.OnCtx(ctx).With(meta.AuditAction, "login_admitted").Select()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d admission rows for one admitted login, want 1", len(rows))
	}
	if !strings.Contains(rows[0].Detail, string(AdmittedByGlobal)) {
		t.Errorf("detail %q does not name the layer that admitted; an operator cannot tell a "+
			"login from shared infrastructure apart from one from a person's own address",
			rows[0].Detail)
	}
	if !strings.Contains(rows[0].Detail, testIP) {
		t.Errorf("detail %q does not name the address", rows[0].Detail)
	}
}

// A login with no admission layer writes no admission row — the record must
// mean something when it is present.
func TestLoginAt_NoLayerMeansNoAdmissionRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, store, _ := newSvc(t)
	if _, _, err := s.Bootstrap(ctx, "johno", rootPass, testIP); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, _, err := s.Login(ctx, "johno", rootPass, testIP); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if n := countAuditRows(t, store, "login_admitted"); n != 0 {
		t.Errorf("%d admission rows for a login with no admission layer; the row would be "+
			"claiming a check that never ran", n)
	}
}

func countAuditRows(t *testing.T, store *meta.Store, action string) uint64 {
	t.Helper()
	n, err := store.Audit.OnCtx(context.Background()).With(meta.AuditAction, action).Count()
	if err != nil {
		t.Fatalf("counting %q: %v", action, err)
	}
	return n
}

func countSessions(t *testing.T, store *meta.Store) uint64 {
	t.Helper()
	n, err := store.Sessions.OnCtx(context.Background()).Count()
	if err != nil {
		t.Fatalf("counting sessions: %v", err)
	}
	return n
}
