package autodocplugin

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/panel"
)

// SYSTEM says the plugin's and the server's versions, and who is signed in.
func TestSystemSaysTheVersions(t *testing.T) {
	p, _ := connected(t, newFakeDB(t))
	got := p.systemText()
	for _, w := range []string{"plugin  v0.0.1 (protocol ", "server  v-fake at ", "user    admin"} {
		if !strings.Contains(got, w) {
			t.Errorf("SYSTEM says %q, wanting %q in it", got, w)
		}
	}
}

// Restart, asked first, stops the server only when idle (sys.restart_if_idle), starts it again and
// resumes the kept sign-in with no form; a store left locked asks the passphrase, the name filled
// in (ADR result 6).
func TestRestartResumesTheSignIn(t *testing.T) {
	for _, locked := range []bool{false, true} {
		db := newFakeDB(t)
		p, _ := signedInOn(t, db)
		p.tab = panel.TabSystem
		p.events().Menu("sys.restart")
		if db.restarts != 0 {
			t.Fatal("Restart ran before its Yes")
		}
		db.mu.Lock()
		db.locked = locked
		db.mu.Unlock()
		p.events().Confirmed()
		<-db.listening()
		if db.restarts != 1 || p.cli == nil {
			t.Fatalf("locked %v: %d restarts, reached again %v (%s)", locked, db.restarts, p.cli != nil, said(p))
		}
		switch {
		case !locked && (p.step != stepPick || p.token != "tok-1"):
			t.Fatalf("after the restart: step %d (%s), want the sign-in resumed", p.step, said(p))
		case locked && (p.step != stepLogin || p.prefill != "admin" || !strings.Contains(p.problem, "locked")):
			t.Fatalf("a locked store: step %d, the name %q (%s)", p.step, p.prefill, said(p))
		}
	}
}

// A server that will not restart says why, and keeps running: busy (what runs), a user who is not
// an administrator, a release before sys.restart_if_idle.
func TestARefusedRestartSaysWhy(t *testing.T) {
	for _, c := range []struct{ answer, want string }{
		{"busy", "1 running a statement, 2 in a transaction"},
		{"denied", "an administrator must restart"},
		{"unknown", "must be restarted by hand"},
	} {
		db := newFakeDB(t)
		db.restart = c.answer
		p, _ := connected(t, db)
		p.events().Menu("sys.restart")
		p.events().Confirmed()
		if db.restarts != 0 || !strings.Contains(p.problem, c.want) || p.step != stepReady {
			t.Errorf("%s: %d restarts, step %d, said %q", c.answer, db.restarts, p.step, p.problem)
		}
	}
}

// A server one protocol behind is found by a probe, never through the working connection; SYSTEM
// opens on it, Upgrade installs the plugin's release, signs in on a lifecycle connection, restarts
// the server once idle and starts the plugin's release (ADR result 11).
func TestAnOlderServerRestartsOntoThePluginsRelease(t *testing.T) {
	db := newFakeDB(t)
	db.protocol, db.nextProtocol = autodb.Protocol-1, autodb.Protocol
	p, _ := testPlugin(t, db)
	opened := dialogs(p)
	p.attach(&fakeHost{})
	if p.step != stepMismatch || p.mis == nil || p.mis.gap != serverBehind || p.tab != panel.TabSystem {
		t.Fatalf("an older server: step %d, %+v, tab %d (%s)", p.step, p.mis, p.tab, said(p))
	}
	if got := enabled(p.systemRows()); !slices.Equal(got, []string{"sys.upgrade", "sys.retry"}) {
		t.Fatalf("the SYSTEM rows: %q", got)
	}
	p.events().Menu("sys.upgrade")
	p.events().Confirmed()
	if db.installs != 1 || !slices.Contains(*opened, panel.DialogSignIn) {
		t.Fatalf("installs %d, dialogs %q (%s)", db.installs, *opened, said(p))
	}
	p.events().SignIn("admin", "right")
	<-db.listening()
	if db.restarts != 1 || p.mis != nil || p.cli == nil || p.bin.Path != autodb.InstallPath(p.pluginDir) {
		t.Fatalf("after the upgrade: %d restarts, mismatch %+v, the binary %+v (%s)", db.restarts, p.mis, p.bin, said(p))
	}
	if p.step != stepLogin {
		t.Fatalf("after the upgrade: step %d, want the sign-in (%s)", p.step, said(p))
	}
}

// An upgrade that cannot restart the server changes nothing but the plugin's bin/: busy says what
// runs; a new release that does not start has the previous binary started again.
func TestAnUpgradeThatCannotFinishSaysSo(t *testing.T) {
	db := newFakeDB(t)
	db.protocol, db.restart = autodb.Protocol-1, "busy"
	p, _ := testPlugin(t, db)
	p.attach(&fakeHost{})
	p.events().Menu("sys.upgrade")
	p.events().Confirmed()
	p.events().SignIn("admin", "right")
	if db.restarts != 0 || !strings.Contains(p.problem, "keeps running") || p.step != stepMismatch {
		t.Fatalf("busy: %d restarts, step %d, said %q", db.restarts, p.step, p.problem)
	}

	db = newFakeDB(t)
	db.protocol = autodb.Protocol - 1
	p, _ = testPlugin(t, db)
	start := p.start
	p.start = func(ctx context.Context, b autodb.Binary) (*autodb.Client, error) {
		if b.Path == autodb.InstallPath(p.pluginDir) {
			return nil, errors.New("exec format error")
		}
		return start(ctx, b)
	}
	p.attach(&fakeHost{})
	p.events().Menu("sys.upgrade")
	p.events().Confirmed()
	p.events().SignIn("admin", "right")
	<-db.listening()
	if p.bin.Path != "autodb" || !strings.Contains(p.problem, "did not start (exec format error)") {
		t.Fatalf("a release that does not start: the binary %+v, said %q", p.bin, p.problem)
	}
}

// A server newer than the plugin: Update asks AutoDoc to update this plugin, by its own name.
func TestANewerServerAsksAutoDocToUpdateThePlugin(t *testing.T) {
	db := newFakeDB(t)
	db.protocol = autodb.Protocol + 1
	p, h := testPlugin(t, db)
	p.attach(h)
	if p.step != stepMismatch || p.mis.gap != pluginBehind {
		t.Fatalf("a newer server: step %d (%s)", p.step, said(p))
	}
	p.events().Menu("sys.update")
	p.events().Confirmed()
	if len(h.calls) != 1 || h.calls[0].id != "plugins.update" || !slices.Equal(h.calls[0].args, []string{"autodb"}) {
		t.Fatalf("host.call: %+v", h.calls)
	}
}

// Sign out ends the session and drops its record.
func TestSignOutDropsTheRecord(t *testing.T) {
	db := newFakeDB(t)
	p, h := signedInOn(t, db)
	p.events().Menu("auth.signout")
	if len(db.took("auth.logout")) != 1 || p.token != "" || p.step != stepLogin || h.slot["t:local:store-a"] != "" {
		t.Fatalf("signed out: logouts %d, step %d, the slot %v", len(db.took("auth.logout")), p.step, h.slot)
	}
}
