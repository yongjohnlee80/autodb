package autodocplugin

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/panel"
	"github.com/yongjohnlee80/autodb/core/remoteclient"
	"github.com/yongjohnlee80/autodb/tui/remotedial"
)

// REMOTE — the remote servers of AutoDB's own remotes.toml (its TUI's too), reached through their
// SSH listeners (tui/remotedial). Connected, CONNECTIONS and NOTES are that server's, and SYSTEM's
// Restart and Upgrade are not offered; Disconnect returns to the local server and its record.
//
// The dial runs on the plugin's goroutine, as every call does, so it asks nothing midway: a first
// connect's host key, unpinned, fails the dial, and the panel asks whether to trust it; a Yes pins
// it in the profile and dials again. An SSH key with a passphrase is asked the same way, and the
// dialer keeps the key it opened for the profile's later dials.

// remoteState is the REMOTE tab's.
type remoteState struct {
	profiles []remoteclient.Profile
	sel      int // the list's selection, -1 for none
	// on is the server connected, nil while local
	on *remoteOn
	// dialers are each profile's, by id: the SSH key a passphrase opened stays in its dialer
	dialers map[string]*remotedial.Dialer
	// keys are the device keys unlocked this run, by profile id: a reconnect resumes its record
	// without the passphrase
	keys map[string]ed25519.PrivateKey
	// keyPass is the SSH key's passphrase for the one dial it was asked for
	keyPass string
}

// remoteOn is a remote server connected.
type remoteOn struct {
	prof remoteclient.Profile
	conn *remotedial.Conn
}

// errKeyPassphrase is the dial stopped to ask the SSH key's passphrase.
var errKeyPassphrase = errors.New("the ssh key's passphrase is wanted")

// remoteTargetOf is a remote server's target: its profile and its store.
func remoteTargetOf(profile, storeID string) string {
	return "t:remote:" + profile + ":" + storeID
}

// targetKey is the target connected: the local server's store, or a remote profile's and store's.
func (p *Plugin) targetKey() string {
	if p.cli == nil {
		return ""
	}
	if on := p.remote.on; on != nil {
		return remoteTargetOf(on.prof.ID, p.cli.Hello.StoreID)
	}
	return localTargetOf(p.cli.Hello.StoreID)
}

// deviceFP is a device key's fingerprint, as a record keeps it.
func deviceFP(k ed25519.PrivateKey) string {
	if len(k) != ed25519.PrivateKeySize {
		return ""
	}
	sum := sha256.Sum256(k.Public().(ed25519.PublicKey))
	return hex.EncodeToString(sum[:8])
}

// loadRemotes reads the profiles and lists them.
func (p *Plugin) loadRemotes() {
	path, err := p.remotesFile()
	if err == nil {
		p.remote.profiles, err = remoteclient.LoadProfiles(path)
	}
	if err != nil {
		p.problem = "the remote servers: " + err.Error()
	}
	if p.remote.sel >= len(p.remote.profiles) {
		p.remote.sel = -1
	}
	items := make([]panel.Item, len(p.remote.profiles))
	for i, pr := range p.remote.profiles {
		label := fmt.Sprintf("%s — %s as %s", profileName(pr), pr.Address(), pr.User)
		if on := p.remote.on; on != nil && on.prof.ID == pr.ID {
			label += " (connected)"
		}
		items[i] = panel.Item{Key: pr.ID, Label: label}
	}
	p.ui.Do(func(v panel.View) { v.Remotes(items) })
}

func (p *Plugin) remotesFile() (string, error) {
	if p.profilesPath != "" {
		return p.profilesPath, nil
	}
	return remoteclient.ProfilesPath()
}

func (p *Plugin) deviceKeyDir() (string, error) {
	if p.keyDir != "" {
		return p.keyDir, nil
	}
	return remoteclient.DeviceKeyDir()
}

func profileName(pr remoteclient.Profile) string {
	if pr.Name != "" {
		return pr.Name
	}
	return pr.ID
}

// remoteText is the REMOTE tab's line.
func (p *Plugin) remoteText() string {
	if on := p.remote.on; on != nil {
		return "connected to " + profileName(on.prof) + " (" + on.prof.Address() + ") as " + on.prof.User
	}
	if len(p.remote.profiles) == 0 {
		return "no remote server yet: Menu › Add server"
	}
	return "local: Enter on a server connects to it"
}

// picked is the profile chosen in the list.
func (p *Plugin) picked() (remoteclient.Profile, bool) {
	if p.remote.sel < 0 || p.remote.sel >= len(p.remote.profiles) {
		return remoteclient.Profile{}, false
	}
	return p.remote.profiles[p.remote.sel], true
}

// remoteRows are REMOTE's menu rows.
func (p *Plugin) remoteRows() []panel.MenuRow {
	pr, ok := p.picked()
	isOn := ok && p.remote.on != nil && p.remote.on.prof.ID == pr.ID
	return []panel.MenuRow{
		{ID: "remote.add", Label: "&Add server…", Enabled: true},
		{ID: "remote.edit", Label: "&Edit…", Enabled: ok && !isOn},
		{ID: "remote.connect", Label: "&Connect", Enabled: ok && !isOn},
		{ID: "remote.disconnect", Label: "Dis&connect", Enabled: p.remote.on != nil},
		{ID: "remote.delete", Label: "&Delete…", Enabled: ok && !isOn},
	}
}

// remoteRow runs a REMOTE row; false for one it does not know.
func (p *Plugin) remoteRow(id string) bool {
	pr, ok := p.picked()
	switch id {
	case "remote.add":
		p.askRemote(nil)
	case "remote.edit":
		if ok {
			p.askRemote(&pr)
		}
	case "remote.connect":
		if ok {
			p.connectRemote(pr)
		}
	case "remote.disconnect":
		p.disconnectRemote()
	case "remote.delete":
		if ok {
			p.askConfirm("Delete server", "Delete "+profileName(pr)+" from the remote servers?", func() { p.deleteRemote(pr) })
		}
	default:
		return false
	}
	return true
}

// askRemote opens the Add or Edit server dialog: old is the profile edited, nil for a new one.
func (p *Plugin) askRemote(old *remoteclient.Profile) {
	f := map[string]any{"remoteTitle": "Add server", "remoteName": "", "remoteHost": "", "remotePort": "",
		"remoteUser": "", "remoteKey": "", "remoteAuthIndex": 0,
		"remoteNote": "a server's SSH listener (autodb --remote); your SSH key is registered there"}
	if old != nil {
		auth := 0
		if old.UseAgent {
			auth = 1
		}
		f = map[string]any{"remoteTitle": "Edit " + profileName(*old), "remoteName": old.Name, "remoteHost": old.Host,
			"remotePort": strconv.Itoa(old.Port), "remoteUser": old.User, "remoteKey": old.KeyFile, "remoteAuthIndex": auth,
			"remoteNote": "changing it ends its kept sign-in"}
	}
	p.onRemote = p.fenced(func(form panel.RemoteForm) { p.saveRemote(old, form) })
	p.ui.Do(func(v panel.View) { v.Fill(f); v.Open(panel.DialogRemote) })
	if p.dialogOpened != nil {
		p.dialogOpened(panel.DialogRemote)
	}
}

// idUnsafe is what a profile id may not hold.
var idUnsafe = regexp.MustCompile(`[^a-z0-9_-]+`)

// saveRemote writes a profile from the dialog: a new one under an id made from its name, or old
// changed. A changed profile is a new target: its records go, and a new host forgets the pinned
// host key.
func (p *Plugin) saveRemote(old *remoteclient.Profile, f panel.RemoteForm) {
	pr := remoteclient.Profile{Name: strings.TrimSpace(f.Name), Host: strings.TrimSpace(f.Host),
		User: strings.TrimSpace(f.User), KeyFile: strings.TrimSpace(f.KeyFile), UseAgent: f.Auth == "agent"}
	if s := strings.TrimSpace(f.Port); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			p.problem = "the port " + s + " is not a number"
			p.draw()
			return
		}
		pr.Port = n
	}
	if old != nil {
		pr.ID = old.ID
		if pr.Host == old.Host && (pr.Port == old.Port || pr.Port == 0 && old.Port == remoteclient.DefaultPort) {
			pr.HostKeyFP = old.HostKeyFP
		}
	} else {
		pr.ID = p.newRemoteID(pr.Name, pr.Host)
	}
	if err := pr.Validate(); err != nil {
		p.problem = strings.TrimPrefix(err.Error(), "remoteclient: ")
		p.draw()
		return
	}
	list := slicesWithout(p.remote.profiles, pr.ID)
	list = append(list, pr)
	if err := p.writeRemotes(list); err != nil {
		return
	}
	if old != nil {
		p.dropRemoteRecords(pr.ID)
		delete(p.remote.dialers, pr.ID)
	}
	p.status, p.problem = "saved "+profileName(pr), ""
	p.loadRemotes()
	p.draw()
}

// newRemoteID is an id for a new profile: its name's (or host's) letters, unique.
func (p *Plugin) newRemoteID(name, host string) string {
	base := strings.Trim(idUnsafe.ReplaceAllString(strings.ToLower(name), "-"), "-_")
	if base == "" {
		base = strings.Trim(idUnsafe.ReplaceAllString(strings.ToLower(host), "-"), "-_")
	}
	if base == "" {
		base = "server"
	}
	base = base[:min(len(base), 56)]
	id := base
	for n := 2; slicesHas(p.remote.profiles, id); n++ {
		id = base + "-" + strconv.Itoa(n)
	}
	return id
}

// deleteRemote removes a profile and its records; its device key stays on this machine.
func (p *Plugin) deleteRemote(pr remoteclient.Profile) {
	if err := p.writeRemotes(slicesWithout(p.remote.profiles, pr.ID)); err != nil {
		return
	}
	p.dropRemoteRecords(pr.ID)
	delete(p.remote.dialers, pr.ID)
	p.status, p.problem = "deleted "+profileName(pr), ""
	p.remote.sel = -1
	p.loadRemotes()
	p.draw()
}

func (p *Plugin) writeRemotes(list []remoteclient.Profile) error {
	path, err := p.remotesFile()
	if err == nil {
		err = remoteclient.SaveProfiles(path, list)
	}
	if err != nil {
		p.problem = "saving the remote servers: " + err.Error()
		p.draw()
	}
	return err
}

// dropRemoteRecords removes every record of profile id.
func (p *Plugin) dropRemoteRecords(id string) {
	prefix := remoteTargetOf(id, "")
	changed := false
	for k := range p.creds {
		if strings.HasPrefix(k, prefix) {
			delete(p.creds, k)
			changed = true
		}
	}
	if changed {
		p.saveCreds()
	}
}

func slicesWithout(list []remoteclient.Profile, id string) []remoteclient.Profile {
	out := make([]remoteclient.Profile, 0, len(list))
	for _, pr := range list {
		if pr.ID != id {
			out = append(out, pr)
		}
	}
	return out
}

func slicesHas(list []remoteclient.Profile, id string) bool {
	for _, pr := range list {
		if pr.ID == id {
			return true
		}
	}
	return false
}

// connectRemote reaches pr: with its device key unlocked this run and a record for it, resumed
// without the passphrase; otherwise the passphrase is asked (twice on this machine's first connect,
// which enrolls its device key).
func (p *Plugin) connectRemote(pr remoteclient.Profile) {
	if key, ok := p.remote.keys[pr.ID]; ok && p.hasRemoteRecord(pr.ID, deviceFP(key)) {
		p.dialRemote(pr, remotedial.Unlock{Key: key}, "")
		return
	}
	first := true
	if dir, err := p.deviceKeyDir(); err == nil {
		if k, err := remoteclient.FilesFor(dir, pr.ID); err == nil {
			first = !k.Exists(k.Key())
		}
	}
	note := "your AutoDB passphrase on " + profileName(pr) + " (" + pr.User + ")"
	if first {
		note += ": the first connect from this machine enrolls it, so type the passphrase twice"
	}
	p.askSecret("Connect to "+profileName(pr), note, first, func(pass, again string) {
		if first && pass != again {
			p.problem = "the two entries differ: Menu › Connect to try again"
			p.draw()
			return
		}
		p.dialRemote(pr, remotedial.Unlock{Passphrase: pass}, pass)
	})
}

// hasRemoteRecord reports whether a record of profile id was made with the device key fp.
func (p *Plugin) hasRemoteRecord(id, fp string) bool {
	for k, r := range p.creds {
		if strings.HasPrefix(k, remoteTargetOf(id, "")) && r.Device == fp && fp != "" {
			return true
		}
	}
	return false
}

// dialer is pr's dialer, kept for the profile so an SSH key opened once is not asked again.
func (p *Plugin) dialer(pr remoteclient.Profile) (*remotedial.Dialer, error) {
	if d, ok := p.remote.dialers[pr.ID]; ok {
		d.Profile = pr
		return d, nil
	}
	dir, err := p.deviceKeyDir()
	if err != nil {
		return nil, err
	}
	keys, err := remoteclient.FilesFor(dir, pr.ID)
	if err != nil {
		return nil, err
	}
	d := &remotedial.Dialer{Profile: pr, Keys: keys, ClientVersion: p.version}
	if p.remote.dialers == nil {
		p.remote.dialers = map[string]*remotedial.Dialer{}
	}
	p.remote.dialers[pr.ID] = d
	return d, nil
}

// dialRemote dials pr and signs in: a record of this profile and store made with this device key
// is resumed first; refused, it goes, and the passphrase signs in (asked, if the dial had none).
func (p *Plugin) dialRemote(pr remoteclient.Profile, u remotedial.Unlock, pass string) {
	d, err := p.dialer(pr)
	if err != nil {
		p.problem = "connecting to " + profileName(pr) + ": " + err.Error()
		p.draw()
		return
	}
	var seen string
	d.ConfirmHostKey = func(fp string) bool { seen = fp; return false }
	keyPass := p.remote.keyPass
	p.remote.keyPass = ""
	d.KeyPassphrase = func() (string, error) {
		if keyPass == "" {
			return "", errKeyPassphrase
		}
		return keyPass, nil
	}
	p.status, p.problem = "connecting to "+pr.Address()+"…", ""
	p.draw()
	ctx, cancel := context.WithTimeout(p.ctx, loginTimeout)
	defer cancel()
	conn, err := d.Dial(ctx, u)
	p.status = ""
	switch {
	case errors.Is(err, remotedial.ErrHostKeyRejected) && seen != "":
		p.askConfirm("Trust the server's host key",
			"The first connect to "+profileName(pr)+". Check this fingerprint against the one its administrator gave you:\n\n"+
				seen+"\n\nTrust it? From now on another key is refused.", func() {
				pr.HostKeyFP = seen
				if p.pinHostKey(pr) == nil {
					p.dialRemote(pr, u, pass)
				}
			})
		return
	case errors.Is(err, errKeyPassphrase), errors.Is(err, remotedial.ErrKeyPassphraseWrong):
		note := "the passphrase of the SSH key " + pr.KeyFile
		if errors.Is(err, remotedial.ErrKeyPassphraseWrong) {
			note = "that passphrase does not open " + pr.KeyFile + ": type it again"
		}
		p.askSecret("The SSH key's passphrase", note, false, func(kp, _ string) {
			p.remote.keyPass = kp
			p.dialRemote(pr, u, pass)
		})
		return
	case err != nil:
		p.problem = "could not connect to " + profileName(pr) + ": " + strings.TrimPrefix(err.Error(), "remotedial: ")
		p.draw()
		return
	}
	storeID, _ := conn.Hello()["store_id"].(string)
	key := remoteTargetOf(pr.ID, storeID)
	device := conn.DeviceKey()
	fp := deviceFP(device)
	token := ""
	if r, ok := p.creds[key]; ok && r.Device == fp {
		if _, err := conn.Resume(ctx, r.Token); err == nil {
			token = r.Token
		} else {
			delete(p.creds, key) // refused: only this record goes
			p.saveCreds()
		}
	}
	if token == "" && pass == "" {
		conn.Close()
		delete(p.remote.keys, pr.ID)
		p.connectRemote(pr) // the passphrase, then
		return
	}
	if token == "" {
		res, err := conn.SignIn(ctx, pass)
		if err != nil {
			conn.Close()
			p.problem = "sign-in on " + profileName(pr) + " refused: " + strings.TrimPrefix(err.Error(), "remotedial: ")
			p.draw()
			return
		}
		token, _ = res["token"].(string)
	}
	if pr.HostKeyFP == "" {
		pr.HostKeyFP = conn.HostKeyFP()
		_ = p.pinHostKey(pr)
	}
	p.switchTarget()
	p.remote.on = &remoteOn{prof: pr, conn: conn}
	if p.remote.keys == nil {
		p.remote.keys = map[string]ed25519.PrivateKey{}
	}
	p.remote.keys[pr.ID] = device
	p.cli = autodb.Over(conn.Client(), conn.Hello(), profileName(pr)+" ("+pr.Address()+")", conn.Close)
	p.token = token
	p.creds[key] = credRecord{User: pr.User, Token: token, Device: fp, At: time.Now().Unix()}
	p.saveCreds()
	p.status = "connected to " + profileName(pr) + " as " + pr.User
	p.loadRemotes()
	p.choose()
}

// pinHostKey writes pr's host key into its profile.
func (p *Plugin) pinHostKey(pr remoteclient.Profile) error {
	list := make([]remoteclient.Profile, len(p.remote.profiles))
	copy(list, p.remote.profiles)
	for i := range list {
		if list[i].ID == pr.ID {
			list[i].HostKeyFP = pr.HostKeyFP
		}
	}
	if err := p.writeRemotes(list); err != nil {
		return err
	}
	p.remote.profiles = list
	return nil
}

// disconnectRemote ends the remote connection, its record kept, and returns to the local server.
func (p *Plugin) disconnectRemote() {
	if p.remote.on == nil {
		return
	}
	name := profileName(p.remote.on.prof)
	p.switchTarget()
	p.loadRemotes()
	p.status = "disconnected from " + name
	if p.bin.Path == "" {
		p.boot()
		return
	}
	p.connect()
}

// switchTarget leaves the server connected: a new target generation, and what was the old one's
// (the tree, the notes, the connection chosen, a table) is dropped. Nothing is logged out.
func (p *Plugin) switchTarget() {
	p.target++
	p.forgetTable(errors.New("the AutoDB server changed"))
	if p.cli != nil {
		_ = p.cli.Close()
	}
	p.cli, p.token, p.sid, p.conn, p.who = nil, "", "", nil, autodb.Who{}
	p.remote.on = nil
	p.tree, p.sel, p.notes, p.mis = nil, nil, noteState{sel: -1}, nil
	p.ui.Do(func(v panel.View) { v.Notes(nil); v.NoteWorkspaces(nil) })
}

// fenced is then, dropped if the target changed since it was asked: an answer for the old server
// never acts on the new one.
func fencedOf[T any](p *Plugin, then func(T)) func(T) {
	gen := p.target
	return func(v T) {
		if p.target != gen {
			p.problem = "the AutoDB server changed since: nothing was done"
			p.draw()
			return
		}
		then(v)
	}
}

func (p *Plugin) fenced(then func(panel.RemoteForm)) func(panel.RemoteForm) {
	return fencedOf(p, then)
}

// askSecret opens the passphrase dialog, asking twice when twice; then is run with the answers.
func (p *Plugin) askSecret(title, note string, twice bool, then func(pass, again string)) {
	gen := p.target
	p.onSecret = func(pass, again string) {
		if p.target != gen {
			p.problem = "the AutoDB server changed since: nothing was done"
			p.draw()
			return
		}
		then(pass, again)
	}
	p.openDialog(panel.DialogSecret, map[string]any{"secretTitle": title, "secretNote": note, "secretTwice": twice})
}
