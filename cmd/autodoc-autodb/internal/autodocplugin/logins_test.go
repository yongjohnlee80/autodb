package autodocplugin

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodoc/plugin"
)

// serverAt is a fake AutoDB serving the meta store store ("" none), taking pass.
func serverAt(t *testing.T, store, pass string) *fakeDB {
	t.Helper()
	db := newFakeDB(t)
	db.storeID, db.pass = store, pass
	return db
}

// openAt is a plugin, its last sign-ins kept at path, opened on db and at the sign-in: a start of
// the plugin as AutoDoc makes one.
func openAt(t *testing.T, db *fakeDB, path string) (*Plugin, *fakeHost) {
	t.Helper()
	p := newPlugin("v0.0.1", t.TempDir())
	p.logins = path
	t.Cleanup(p.Close)
	p.find = func(context.Context) (autodb.Binary, []autodb.Tried, error) {
		return autodb.Binary{Path: "autodb", Version: "v-fake", Via: "test"}, nil, nil
	}
	p.start = func(ctx context.Context, _ autodb.Binary) (*autodb.Client, error) {
		return autodb.Dial(ctx, "tcp", db.addr, "test")
	}
	h := &fakeHost{}
	p.attach(h, 60, 24)
	if p.step != stepLogin {
		t.Fatalf("after open the card is at step %d, want the sign-in (%s)", p.step, h.card())
	}
	return p, h
}

// signInAs types name over the one in the field, then pass, and presses Enter on the passphrase.
func signInAs(p *Plugin, name, pass string) {
	p.focus(focusName)
	p.Key(plugin.Key{Key: "u", Ctrl: true})
	typeText(p, name)
	tab(p)
	typeText(p, pass)
	p.Key(plugin.Key{Key: "Enter"})
}

// signedInAs opens a plugin on db and signs in as name, which db takes.
func signedInAs(t *testing.T, db *fakeDB, path, name, pass string) {
	t.Helper()
	p, h := openAt(t, db, path)
	signInAs(p, name, pass)
	if p.step != stepPick {
		t.Fatalf("signing in as %s: the card is at step %d (%s)", name, p.step, h.card())
	}
}

// record is the file at path, read as the plugin writes it.
func record(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("the record %q is not a JSON object of names: %v", b, err)
	}
	return m
}

// wantPrefilled is the sign-in opened with name typed in and the focus on the passphrase; "" is
// an empty name with the focus on it.
func wantPrefilled(t *testing.T, p *Plugin, h *fakeHost, name string) {
	t.Helper()
	if got := p.nameIn.Value(); got != name {
		t.Fatalf("the sign-in opened with the name %q, want %q", got, name)
	}
	if name == "" {
		if p.field() != focusName {
			t.Fatalf("an empty name opened with the focus on %d, want the name", p.field())
		}
		return
	}
	if p.field() != focusPass {
		t.Fatalf("the name %q opened with the focus on %d, want the passphrase", name, p.field())
	}
	if card := h.card(); !strings.Contains(card, name) || !strings.Contains(card, "Enter: sign in") {
		t.Fatalf("the card does not show %q with the passphrase focused:\n%s", name, card)
	}
}

// A first sign-in, with nothing recorded, starts with an empty name, focused.
func TestAFirstSignInStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", loginsFile)
	p, h := openAt(t, serverAt(t, "store-a", freshPass(t)), path)
	wantPrefilled(t, p, h, "")
	if p.problem != "" {
		t.Fatalf("a first sign-in says %q", p.problem)
	}
}

// After a sign-in, the next start of the plugin opens the sign-in to that server with the name
// typed in and the focus on the passphrase, so the passphrase and Enter sign in. The server is
// its meta store: the same store answering at another address is the same server. A server that
// names no store is its address.
func TestARestartPrefillsTheLastName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", loginsFile)
	pass := freshPass(t)
	signedInAs(t, serverAt(t, "store-a", pass), path, "alice", pass)

	again := serverAt(t, "store-a", pass) // restarted, at another address
	p, h := openAt(t, again, path)
	wantPrefilled(t, p, h, "alice")
	typeText(p, pass)
	p.Key(plugin.Key{Key: "Enter"})
	if p.step != stepPick {
		t.Fatalf("the passphrase and Enter did not sign in: step %d (%s)", p.step, h.card())
	}
	if l := again.took("auth.login"); len(l) != 1 || l[0].params[0] != "alice" {
		t.Fatalf("auth.login took %+v, want alice", l)
	}

	bare := serverAt(t, "", pass)
	signedInAs(t, bare, path, "bob", pass)
	p, h = openAt(t, bare, path)
	wantPrefilled(t, p, h, "bob")
	want := map[string]string{"store:store-a": "alice", "addr:" + bare.addr: "bob"}
	if got := record(t, path); !maps.Equal(got, want) {
		t.Fatalf("the record is %v, want %v", got, want)
	}
}

// Each server keeps the name signed in to it, and one never signed in to starts empty.
func TestEachServerKeepsItsOwnName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", loginsFile)
	pass := freshPass(t)
	signedInAs(t, serverAt(t, "store-a", pass), path, "alice", pass)
	signedInAs(t, serverAt(t, "store-b", pass), path, "bob", pass)

	for _, c := range []struct{ store, name string }{{"store-a", "alice"}, {"store-b", "bob"}, {"store-c", ""}} {
		p, h := openAt(t, serverAt(t, c.store, pass), path)
		wantPrefilled(t, p, h, c.name)
	}
	want := map[string]string{"store:store-a": "alice", "store:store-b": "bob"}
	if got := record(t, path); !maps.Equal(got, want) {
		t.Fatalf("the record is %v, want %v", got, want)
	}
}

// A refused sign-in changes nothing: no record is made, and the one there stays, byte for byte.
func TestAFailedSignInLeavesTheRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", loginsFile)
	pass := freshPass(t)

	p, h := openAt(t, serverAt(t, "store-a", pass), path)
	signInAs(p, "mallory", "wrong")
	if p.step != stepLogin {
		t.Fatalf("a wrong passphrase signed in: step %d (%s)", p.step, h.card())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a refused first sign-in left a record (%v)", err)
	}

	signedInAs(t, serverAt(t, "store-a", pass), path, "alice", pass)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p, h = openAt(t, serverAt(t, "store-a", pass), path)
	signInAs(p, "mallory", "wrong")
	if p.step != stepLogin {
		t.Fatalf("a wrong passphrase signed in: step %d (%s)", p.step, h.card())
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
		t.Fatalf("a refused sign-in changed the record from %q to %q (%v)", before, after, err)
	}
	p, h = openAt(t, serverAt(t, "store-a", pass), path)
	wantPrefilled(t, p, h, "alice")
}

// A record that cannot be read counts as none: the name starts empty, the card says why once, and
// the sign-in still signs in. A record that is not JSON is then replaced by a good one.
func TestARecordThatCannotBeReadNeverBlocksTheSignIn(t *testing.T) {
	pass := freshPass(t)
	t.Run("corrupt", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), loginsFile)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"store:store-a": "alice"`), 0o600); err != nil {
			t.Fatal(err)
		}
		p, h := openAt(t, serverAt(t, "store-a", pass), path)
		wantPrefilled(t, p, h, "")
		if card := h.card(); !strings.Contains(card, "could not be read") {
			t.Fatalf("the card does not say the record could not be read:\n%s", card)
		}
		signInAs(p, "alice", pass)
		if p.step != stepPick {
			t.Fatalf("a corrupt record blocked the sign-in: step %d (%s)", p.step, h.card())
		}
		if got := record(t, path); !maps.Equal(got, map[string]string{"store:store-a": "alice"}) {
			t.Fatalf("the corrupt record was replaced by %v", got)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), loginsFile)
		if err := os.MkdirAll(path, 0o700); err != nil { // a directory where the file goes
			t.Fatal(err)
		}
		p, h := openAt(t, serverAt(t, "store-a", pass), path)
		wantPrefilled(t, p, h, "")
		if card := h.card(); !strings.Contains(card, "could not be read") {
			t.Fatalf("the card does not say the record could not be read:\n%s", card)
		}
		signInAs(p, "alice", pass)
		if p.step != stepPick {
			t.Fatalf("an unreadable record blocked the sign-in: step %d (%s)", p.step, h.card())
		}
		if card := h.card(); !strings.Contains(card, "could not be kept") {
			t.Fatalf("the card does not say the name could not be kept:\n%s", card)
		}
		if strings.Contains(h.card(), pass) {
			t.Fatal("the card shows the passphrase")
		}
	})
}

// The record holds the name and nothing else: not the passphrase, not the token.
func TestTheRecordHoldsNoSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", loginsFile)
	pass := freshPass(t)
	p, h := openAt(t, serverAt(t, "store-a", pass), path)
	signInAs(p, "alice", pass)
	if p.step != stepPick || p.token == "" {
		t.Fatalf("the sign-in did not sign in: step %d (%s)", p.step, h.card())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), pass) {
		t.Fatal("the record holds the passphrase")
	}
	if strings.Contains(string(b), p.token) {
		t.Fatal("the record holds the token")
	}
	if got := record(t, path); !maps.Equal(got, map[string]string{"store:store-a": "alice"}) {
		t.Fatalf("the record is %v, want the name alone", got)
	}
}

// The record is mode 0600 in a directory of 0700, and a new record is a new file renamed over the
// old, never the old one rewritten in place; no temporary file is left beside it.
func TestTheRecordIsPrivateAndReplacedWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", loginsFile)
	pass := freshPass(t)
	private := func() os.FileInfo {
		t.Helper()
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if m := fi.Mode().Perm(); m != 0o600 {
			t.Fatalf("the record's mode is %o, want 600", m)
		}
		di, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if m := di.Mode().Perm(); m != 0o700 {
			t.Fatalf("the record's directory's mode is %o, want 700", m)
		}
		ents, err := os.ReadDir(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if len(ents) != 1 || ents[0].Name() != filepath.Base(path) {
			var names []string
			for _, e := range ents {
				names = append(names, e.Name())
			}
			t.Fatalf("the record's directory holds %v, want the record alone", names)
		}
		return fi
	}
	signedInAs(t, serverAt(t, "store-a", pass), path, "alice", pass)
	first := private()
	signedInAs(t, serverAt(t, "store-a", pass), path, "bob", pass)
	second := private()
	if os.SameFile(first, second) {
		t.Fatal("the new record was written into the old file in place, not renamed over it")
	}
	if got := record(t, path); !maps.Equal(got, map[string]string{"store:store-a": "bob"}) {
		t.Fatalf("the record is %v, want bob", got)
	}
}

// The record lives under $XDG_STATE_HOME, or ~/.local/state without it.
func TestTheRecordIsState(t *testing.T) {
	env := func(state string) func(string) string {
		return func(k string) string {
			if k == "XDG_STATE_HOME" {
				return state
			}
			return ""
		}
	}
	if got, want := loginsPath(env("/s")), filepath.Join("/s", "autodoc-autodb", "logins.json"); got != want {
		t.Fatalf("with XDG_STATE_HOME the record is %s, want %s", got, want)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, want := loginsPath(env("")), filepath.Join(home, ".local", "state", "autodoc-autodb", "logins.json"); got != want {
		t.Fatalf("without XDG_STATE_HOME the record is %s, want %s", got, want)
	}
}
