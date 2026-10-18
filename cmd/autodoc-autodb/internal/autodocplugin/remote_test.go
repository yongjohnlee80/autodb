package autodocplugin

import (
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/panel"
	"github.com/yongjohnlee80/autodb/core/remoteclient"
	"github.com/yongjohnlee80/autodb/internal/remotetest"
)

// remoteSetup is a plugin signed in to a local fake (store-a), with AutoDB's remote fixture added
// as the server "Office" and chosen in REMOTE.
func remoteSetup(t *testing.T) (*Plugin, *fakeHost, *fakeDB, *remotetest.Server) {
	t.Helper()
	s := remotetest.Start(t)
	_, keys := s.Profile(t)
	db := newFakeDB(t)
	db.storeID = "store-a"
	p, h := testPlugin(t, db)
	p.profilesPath = filepath.Join(t.TempDir(), "remotes.toml")
	p.keyDir = keys.Dir
	p.attach(h)
	signIn(p, "right")
	host, port, _ := net.SplitHostPort(s.Addr)
	p.tab = panel.TabRemote
	p.events().Menu("remote.add")
	p.events().RemoteSaved(panel.RemoteForm{Name: "Office", Host: host, Port: port, User: "alice", KeyFile: s.KeyFile, Auth: "file"})
	if len(p.remote.profiles) != 1 || p.remote.profiles[0].ID != "office" {
		t.Fatalf("the server added: %+v (%s)", p.remote.profiles, said(p))
	}
	p.events().RemoteCurrent(0)
	return p, h, db, s
}

// A remote server is added, connected (the passphrase twice on this machine's first connect, the
// host key trusted and pinned), and its connections are the tabs'; Restart is not offered for it;
// Disconnect returns to the local server, signed in, its record never overwritten.
func TestARemoteServerIsConnectedAndLeft(t *testing.T) {
	p, h, _, s := remoteSetup(t)
	opened := dialogs(p)
	p.events().Menu("remote.connect")
	p.events().Secret(remotetest.AlicePass, remotetest.AlicePass)
	if !slices.Equal(*opened, []string{panel.DialogSecret, panel.DialogConfirm}) || p.remote.on != nil {
		t.Fatalf("a first connect: dialogs %q, connected %v (%s)", *opened, p.remote.on != nil, said(p))
	}
	p.events().Confirmed() // the host key: trusted
	if p.remote.on == nil || p.step != stepPick || p.who.Name != "alice" {
		t.Fatalf("after trusting the host key: connected %v, step %d, user %q (%s)", p.remote.on != nil, p.step, p.who.Name, said(p))
	}
	saved, err := remoteclient.LoadProfiles(p.profilesPath)
	if err != nil || len(saved) != 1 || saved[0].HostKeyFP != s.HostFP {
		t.Fatalf("the host key is not pinned: %+v (%v)", saved, err)
	}
	remoteKey := remoteTargetOf("office", p.cli.Hello.StoreID)
	if h.slot["t:local:store-a"] == "" || !strings.Contains(h.slot[remoteKey], `"device":"`) {
		t.Fatalf("the slot after connecting: %v", h.slot)
	}
	if !slices.ContainsFunc(p.tree.workspaces, func(w autodb.Workspace) bool { return w.ID == s.Workspace }) {
		t.Fatalf("the tree is not the remote's: %+v", p.tree.workspaces)
	}
	if slices.Contains(enabled(p.systemRows()), "sys.restart") {
		t.Fatal("Restart is offered for a remote server")
	}

	p.events().Menu("remote.disconnect")
	if p.remote.on != nil || p.step != stepPick || p.token != "tok-1" || h.slot[remoteKey] == "" {
		t.Fatalf("after Disconnect: remote %v, step %d, local token %v, the slot %v (%s)", p.remote.on != nil, p.step,
			p.token == "tok-1", h.slot, said(p))
	}
}

// A wrong passphrase is refused and leaves the local server as it was.
func TestARefusedRemoteSignInKeepsTheLocalServer(t *testing.T) {
	p, _, _, s := remoteSetup(t)
	saved, _ := remoteclient.LoadProfiles(p.profilesPath)
	saved[0].HostKeyFP = s.HostFP // pinned already: no question
	if err := remoteclient.SaveProfiles(p.profilesPath, saved); err != nil {
		t.Fatal(err)
	}
	p.loadRemotes()
	p.events().Menu("remote.connect")
	p.events().Secret("not alice's passphrase", "not alice's passphrase")
	if p.remote.on != nil || p.token != "tok-1" || !strings.Contains(p.problem, "refused") {
		t.Fatalf("a wrong passphrase: connected %v, local token %v (%s)", p.remote.on != nil, p.token == "tok-1", said(p))
	}
}

// Edit keeps the profile's id; a new host forgets its pinned key and its records go. Delete asks
// first.
func TestARemoteServerIsEditedAndDeleted(t *testing.T) {
	p, h, _, _ := remoteSetup(t)
	p.creds[remoteTargetOf("office", "s1")] = credRecord{User: "alice", Token: "t", Device: "d"}
	p.remote.profiles[0].HostKeyFP = "SHA256:old"
	p.events().Menu("remote.edit")
	p.events().RemoteSaved(panel.RemoteForm{Name: "Office", Host: "elsewhere.example", Port: "7422", User: "alice", Auth: "agent"})
	saved, _ := remoteclient.LoadProfiles(p.profilesPath)
	if len(saved) != 1 || saved[0].ID != "office" || saved[0].HostKeyFP != "" || !saved[0].UseAgent {
		t.Fatalf("after Edit: %+v", saved)
	}
	if _, ok := h.slot[remoteTargetOf("office", "s1")]; ok {
		t.Fatalf("the edited server's record stayed: %v", h.slot)
	}
	p.events().Menu("remote.delete")
	if saved, _ := remoteclient.LoadProfiles(p.profilesPath); len(saved) != 1 {
		t.Fatal("Delete ran before its Yes")
	}
	p.events().Confirmed()
	if saved, _ := remoteclient.LoadProfiles(p.profilesPath); len(saved) != 0 {
		t.Fatalf("after Delete: %+v", saved)
	}
}

// An answer asked of one server never acts on another: a Delete confirmed after the target
// changed does nothing.
func TestAnAnswerForTheOldServerIsDropped(t *testing.T) {
	db := newFakeDB(t)
	p, _ := connected(t, db)
	p.events().Current([]string{"w1", "c7"})
	p.events().Menu("conn.delete")
	p.switchTarget()
	p.events().Confirmed()
	if len(db.took("conn.delete")) != 0 || !strings.Contains(p.problem, "changed since") {
		t.Fatalf("conn.delete took %+v (%s)", db.took("conn.delete"), said(p))
	}
}
