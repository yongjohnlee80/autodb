package autodocplugin

import (
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodoc/plugin"
)

// signedInOn is a plugin signed in to db (store "store-a"), its slot as AutoDoc keeps it.
func signedInOn(t *testing.T, db *fakeDB) (*Plugin, *fakeHost) {
	t.Helper()
	db.storeID = "store-a"
	p, h := testPlugin(t, db)
	p.attach(h)
	signIn(p, "right")
	if p.step != stepPick || h.slot["t:local:store-a"] == "" || h.slot["v"] != slotVersion {
		t.Fatalf("after signing in: step %d, the slot %v", p.step, h.slot)
	}
	return p, h
}

// restartWith is the plugin started again with the slot AutoDoc kept: Open.Session.
func restartWith(t *testing.T, db *fakeDB, slot map[string]string) (*Plugin, *fakeHost) {
	t.Helper()
	p, h := testPlugin(t, db)
	p.creds = readCreds(slot) // what Open does with o.Session
	p.attach(h)
	return p, h
}

// A sign-in kept in the slot is resumed at the next start with no form, checked by auth.whoami;
// no passphrase is asked again.
func TestAKeptSignInResumes(t *testing.T) {
	db := newFakeDB(t)
	_, h := signedInOn(t, db)
	logins, asked := len(db.took("auth.login")), len(db.took("auth.whoami"))
	p, _ := restartWith(t, db, h.slot)
	if p.step != stepPick || p.token != "tok-1" || len(db.took("auth.login")) != logins {
		t.Fatalf("the restart: step %d, signed in %v, logins %d → %d (%s)", p.step, p.token != "", logins,
			len(db.took("auth.login")), said(p))
	}
	if w := len(db.took("auth.whoami")) - asked; w != 1 {
		t.Fatalf("the restart asked auth.whoami %d times, want once", w)
	}
}

// A kept token the server refuses (expired, revoked) is dropped from the slot, and the sign-in
// opens; one of another store is not tried.
func TestARefusedOrForeignRecordAsksAgain(t *testing.T) {
	db := newFakeDB(t)
	_, h := signedInOn(t, db)
	db.revoked = true
	p, h2 := restartWith(t, db, h.slot)
	if p.step != stepLogin || p.token != "" || h2.slot["t:local:store-a"] != "" {
		t.Fatalf("a refused token: step %d, signed in %v, the slot %v", p.step, p.token != "", h2.slot)
	}

	db.revoked = false
	other := newFakeDB(t)
	other.storeID = "store-b"
	before := len(other.took("auth.whoami"))
	q, _ := restartWith(t, other, h.slot)
	if q.step != stepLogin || len(other.took("auth.whoami")) != before {
		t.Fatalf("another store's record: step %d, whoami asked %d times", q.step, len(other.took("auth.whoami"))-before)
	}
}

// A valid token on a store restarted without its service keyslot (locked): the sign-in opens,
// the name filled in, saying the store is locked.
func TestALockedStoreAsksThePassphraseWithTheNameFilled(t *testing.T) {
	db := newFakeDB(t)
	_, h := signedInOn(t, db)
	db.locked = true
	p, _ := restartWith(t, db, h.slot)
	if p.step != stepLogin || p.token != "" || p.prefill != "admin" {
		t.Fatalf("a locked store: step %d, signed in %v, the name %q", p.step, p.token != "", p.prefill)
	}
	if !strings.Contains(said(p), "store is locked") {
		t.Fatalf("the panel does not say the store is locked: %s", said(p))
	}
	db.locked = false
	signIn(p, "right")
	if p.step != stepPick {
		t.Fatalf("the passphrase did not sign in: %s", said(p))
	}
}

// Closing as AutoDoc turns the plugin off or updates it keeps the sign-in (no logout); closing as
// it quits, or with no reason given, logs out.
func TestTheCloseReasonDecidesTheLogout(t *testing.T) {
	for _, c := range []struct {
		reason string
		logout bool
	}{{plugin.CloseOff, false}, {plugin.CloseUpdate, false}, {plugin.CloseQuit, true}, {plugin.CloseRemove, true}, {"", true}} {
		db := newFakeDB(t)
		p, h := signedInOn(t, db)
		h.reason = c.reason
		p.Close()
		if got := len(db.took("auth.logout")) == 1; got != c.logout {
			t.Errorf("closed for %q: logged out %v, want %v", c.reason, got, c.logout)
		}
	}
}
