package tui

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	tuidecl "github.com/yongjohnlee80/golib/tui/decl"

	"github.com/yongjohnlee80/autodb/core/remoteclient"
)

// THE MANAGE DIALOG — Remote › Manage….
//
// One dialog, its sections chosen at the top: each is offered only where it
// applies, and its table and buttons are shown only while it is chosen.
//
//   - Servers is this computer's: the profiles in remotes.toml and the device
//     keys beside them. Offered where the frontend owns its session.
//   - My devices is the signed-in user's SSH keys on the connected server —
//     the local daemon or the remote one, whichever the session is on — and
//     the device each is bound to. Home › My SSH keys… opens it.
//
// A profile the session is connected to is not changed under it: Disconnect
// first. Revoking the key or device this computer is connected with ends
// this session, so it goes back to the local daemon instead of reconnecting
// with a device the server now refuses (which would count against this
// network).

const (
	sectionServers = "servers"
	sectionMine    = "mine"
)

type manageSection struct{ id, label string }

func manageState(h *Host) map[string]any {
	return map[string]any{
		"App.manageSections":     h.manageSectionModel,
		"App.manageSectionIndex": -1,
		"App.manageStatus":       "",
		"App.manageOnServers":    false,
		"App.manageOnMine":       false,
		"App.manageMineTitle":    "",
		"App.serverRows":         h.serverRows,
		"App.mineRows":           h.mine.model,
		"App.serverFormTitle":    "",
		"App.serverFormError":    "",
		"App.serverFormName":     "",
		"App.serverFormHost":     "",
		"App.serverFormPort":     "",
		"App.serverFormUser":     "",
		"App.serverFormKeyFile":  "",
		"App.serverAuthChoices":  h.serverAuthChoices,
		"App.serverFormAuth":     -1,
		"App.serverFormPin":      "",
		"App.keyFormTitle":       "",
		"App.keyFormError":       "",
		"App.keyFormAdding":      false,
		"App.keyFormLabel":       "",
		"App.keyFormPublic":      "",
	}
}

// serverAuthModel is how a profile's SSH key signs: the key file itself, or
// the agent holding it.
func serverAuthModel() *tuidecl.ListModel {
	m := tuidecl.NewListModel("key", "id", "label")
	m.Reset([]tuidecl.Row{
		{"key": "file", "id": "file", "label": "the key file (asks for its passphrase if it has one)"},
		{"key": "agent", "id": "agent", "label": "ssh-agent (the key file's .pub names the key)"},
	})
	return m
}

// newMyKeysManager is My devices: the caller's SSH keys on the connected
// server, each with its device.
func newMyKeysManager(h *Host) *manager[SSHKeyRow] {
	return newManager("App.manageStatus",
		func(ctx context.Context, b *Bound) ([]SSHKeyRow, error) { return b.SSHKeys(ctx, 0) },
		func(k SSHKeyRow) tuidecl.Row {
			device, lastIP, lastSeen := "none yet", "", ""
			if k.Device != nil {
				device = "enrolled " + shortDate(k.Device.EnrolledAt)
				lastIP, lastSeen = k.Device.LastIP, shortDate(k.Device.LastSeenAt)
			}
			if h.connectedWith(k.Fingerprint) {
				device = "this computer"
			}
			label := k.Label
			if label == "" {
				label = "(no label)"
			}
			return tuidecl.Row{"key": strconv.FormatInt(k.ID, 10), "label": label,
				"fingerprint": shortFP(k.Fingerprint), "type": k.Type, "device": device,
				"lastIP": lastIP, "lastSeen": lastSeen}
		}, "key", "label", "fingerprint", "type", "device", "lastIP", "lastSeen")
}

// shortDate is a unix time as a date and minute, "" for none.
func shortDate(unix int64) string {
	if unix == 0 {
		return ""
	}
	return time.Unix(unix, 0).Format("2006-01-02 15:04")
}

// shortFP shortens a SHA256 fingerprint for a table column.
func shortFP(fp string) string {
	if len(fp) > 19 {
		return fp[:19] + "…"
	}
	return fp
}

// connectedWith reports whether the remote connection in use authenticated
// with the SSH key of fingerprint fp: that key's device is this computer.
func (h *Host) connectedWith(fp string) bool {
	own := h.session.RemoteSSHKeyFP()
	return own != "" && own == fp
}

// connectedServer is where My devices' keys are: the remote profile, or the
// local daemon.
func (h *Host) connectedServer() string {
	if h.session.Remote() {
		return h.remoteProfileName()
	}
	return "the local server"
}

// manageSections are the sections offered to this session now.
func (h *Host) manageSections() []manageSection {
	var out []manageSection
	if h.ownsConnection() {
		out = append(out, manageSection{sectionServers, "Servers — the remote servers this computer knows"})
	}
	if h.session.Token() != "" {
		out = append(out, manageSection{sectionMine, "My devices — your SSH keys on " + h.connectedServer()})
	}
	return out
}

// openManage is remote.manage (and Home › My SSH keys…, at sectionMine):
// the dialog, at section when it is offered, else at the first.
func (h *Host) openManage(section string) {
	h.sections = h.manageSections()
	if len(h.sections) == 0 {
		h.setStatus("nothing to manage here")
		return
	}
	rows := make([]tuidecl.Row, len(h.sections))
	at := 0
	for i, s := range h.sections {
		rows[i] = tuidecl.Row{"key": s.id, "label": s.label}
		if s.id == section {
			at = i
		}
	}
	h.manageSectionModel.Reset(rows)
	h.set("App.manageSectionIndex", -1)
	h.set("App.manageSectionIndex", at)
	h.showSection(at)
	h.open("remoteManage")
}

// manageSectionChosen is App.manageSection(i): another section chosen.
func (h *Host) manageSectionChosen(i int) error {
	if i >= 0 && i < len(h.sections) {
		h.showSection(i)
	}
	return nil
}

// showSection shows section i, loading it.
func (h *Host) showSection(i int) {
	h.section = h.sections[i].id
	if err := h.p.SetMany(map[string]any{
		"App.manageOnServers": h.section == sectionServers,
		"App.manageOnMine":    h.section == sectionMine,
		"App.manageStatus":    "",
	}); err != nil {
		h.keep(err)
		return
	}
	switch h.section {
	case sectionServers:
		h.loadServers("")
	case sectionMine:
		h.set("App.manageMineTitle", "your SSH keys on "+h.connectedServer()+", and the device each is bound to")
		h.mine.all, h.mine.rows = nil, nil
		h.mine.model.Reset(nil)
		openManager(h, h.mine)
	}
}

// on reports whether the dialog is at section: a button of another section
// (hidden, but its key still reaches the dialog) does nothing.
func (h *Host) on(section string) bool { return h.section == section }

// manageClosed is App.manageClosed.
func (h *Host) manageClosed() error {
	h.section = ""
	h.mine.bound = nil
	h.serverFormSeq++
	h.keyFormBound = nil
	return nil
}

// manageRefresh is App.manageRefresh: the section again.
func (h *Host) manageRefresh() error {
	switch h.section {
	case sectionServers:
		h.loadServers("")
	case sectionMine:
		if h.mine.bound != nil {
			reloadManager(h, h.mine, "")
		}
	}
	return nil
}

// ---- Servers ---------------------------------------------------------------

// loadServers reads remotes.toml into the Servers table, and then says done.
func (h *Host) loadServers(done string) {
	path, err := h.profilesPath()
	if err == nil {
		h.servers, err = remoteclient.LoadProfiles(path)
	}
	if err != nil {
		h.servers = nil
		h.serverRows.Reset(nil)
		h.set("App.manageStatus", "remote profiles: "+err.Error())
		return
	}
	connected, _ := h.session.RemoteProfile()
	rows := make([]tuidecl.Row, len(h.servers))
	for i, p := range h.servers {
		name := p.Name
		if name == "" {
			name = p.ID
		}
		if h.session.Remote() && connected.ID == p.ID {
			name += " (connected)"
		}
		keyText := p.KeyFile
		if p.UseAgent {
			keyText += " (agent)"
		}
		pin := "not yet"
		if p.HostKeyFP != "" {
			pin = shortFP(p.HostKeyFP)
		}
		rows[i] = tuidecl.Row{"key": p.ID, "name": name, "address": p.Address(), "user": p.User,
			"sshKey": keyText, "pin": pin, "device": h.deviceState(p)}
	}
	h.serverRows.Reset(rows)
	if done == "" && len(h.servers) == 0 {
		done = "no remote servers yet — Add one"
	}
	h.set("App.manageStatus", done)
}

// deviceState is whether this computer holds a device key for p.
func (h *Host) deviceState(p remoteclient.Profile) string {
	k, err := h.keysFor(p)
	if err != nil {
		return "?"
	}
	switch {
	case k.Exists(k.Key()) || k.Exists(k.Next()):
		return "enrolled"
	case k.Exists(k.Pending()):
		return "enrolling"
	}
	return "none yet"
}

func (h *Host) keysFor(p remoteclient.Profile) (remoteclient.KeyFiles, error) {
	dir, err := h.keyDir()
	if err != nil {
		return remoteclient.KeyFiles{}, err
	}
	return remoteclient.FilesFor(dir, p.ID)
}

// serverAt is Servers row i, refused while the session is connected to it.
func (h *Host) serverAt(i int) (remoteclient.Profile, bool) {
	if i < 0 || i >= len(h.servers) {
		h.set("App.manageStatus", "choose a server first")
		return remoteclient.Profile{}, false
	}
	p := h.servers[i]
	if connected, ok := h.session.RemoteProfile(); ok && connected.ID == p.ID {
		h.set("App.manageStatus", "connected to "+profileLabel(p)+" — Disconnect first")
		return remoteclient.Profile{}, false
	}
	return p, true
}

func profileLabel(p remoteclient.Profile) string {
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}

// serverAdd is App.serverAdd: the form, empty but for the defaults.
func (h *Host) serverAdd() error {
	if h.on(sectionServers) {
		h.showServerForm("", "add a remote server", remoteclient.Profile{
			Port: remoteclient.DefaultPort, KeyFile: remoteclient.DefaultKeyFile})
	}
	return nil
}

// serverEdit is App.serverEdit(i).
func (h *Host) serverEdit(i int) error {
	if !h.on(sectionServers) {
		return nil
	}
	if p, ok := h.serverAt(i); ok {
		h.showServerForm(p.ID, "edit "+profileLabel(p), p)
	}
	return nil
}

func (h *Host) showServerForm(id, title string, p remoteclient.Profile) {
	h.serverFormSeq++
	h.serverFormID = id
	auth := 0
	if p.UseAgent {
		auth = 1
	}
	pin := "not pinned yet: the first connect asks you to confirm it"
	if p.HostKeyFP != "" {
		pin = p.HostKeyFP
	}
	port := ""
	if p.Port != 0 {
		port = strconv.Itoa(p.Port)
	}
	values := map[string]any{
		"App.serverFormTitle": title, "App.serverFormError": "", "App.serverFormPin": pin,
		"App.serverFormName": p.Name, "App.serverFormHost": p.Host, "App.serverFormPort": port,
		"App.serverFormUser": p.User, "App.serverFormKeyFile": p.KeyFile,
	}
	h.refill(values)
	h.set("App.serverFormAuth", -1)
	h.set("App.serverFormAuth", auth)
	h.open("remoteServerForm")
}

// refill sets a form's text sources, through a different value first, so a
// field edited since the last open is set back even when the value is the
// same.
func (h *Host) refill(values map[string]any) {
	blank := map[string]any{}
	for k, v := range values {
		if s, ok := v.(string); ok {
			blank[k] = s + " "
		}
	}
	if err := h.p.SetMany(blank); err != nil {
		h.keep(err)
	}
	if err := h.p.SetMany(values); err != nil {
		h.keep(err)
	}
}

// serverFormClosed is App.serverFormClosed: the form cancelled.
func (h *Host) serverFormClosed() error {
	h.serverFormSeq++
	return nil
}

// refuseServerForm opens the form again with what was typed, saying why.
func (h *Host) refuseServerForm(why string, typed map[string]any) error {
	typed["App.serverFormError"] = why
	if err := h.p.SetMany(typed); err != nil {
		h.keep(err)
	}
	h.p.Post(func() { h.open("remoteServerForm") })
	return nil
}

var profileIDUnsafe = regexp.MustCompile(`[^a-z0-9_-]+`)

// newProfileID is an id for a new profile named name (or at host), unused
// among profiles.
func newProfileID(name, host string, profiles []remoteclient.Profile) string {
	base := strings.Trim(profileIDUnsafe.ReplaceAllString(strings.ToLower(name), "-"), "-_")
	if base == "" {
		base = strings.Trim(profileIDUnsafe.ReplaceAllString(strings.ToLower(host), "-"), "-_")
	}
	if base == "" || !(base[0] >= 'a' && base[0] <= 'z' || base[0] >= '0' && base[0] <= '9') {
		base = "server" + base
	}
	if len(base) > 56 {
		base = base[:56]
	}
	taken := map[string]bool{}
	for _, p := range profiles {
		taken[p.ID] = true
	}
	id := base
	for n := 2; taken[id]; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	return id
}

// saveServer is App.saveServer(name, host, port, user, keyFile, auth): the
// form answered.
func (h *Host) saveServer(name, host, portText, user, keyFile, auth string) error {
	typed := map[string]any{"App.serverFormName": name, "App.serverFormHost": host,
		"App.serverFormPort": portText, "App.serverFormUser": user, "App.serverFormKeyFile": keyFile}
	host, user, keyFile = strings.TrimSpace(host), strings.TrimSpace(user), strings.TrimSpace(keyFile)
	if host == "" {
		return h.refuseServerForm("the server's host name or address is required", typed)
	}
	port := 0
	if t := strings.TrimSpace(portText); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 1 || n > 65535 {
			return h.refuseServerForm("the port is a number from 1 to 65535 (blank for 7422)", typed)
		}
		port = n
	}
	if user == "" {
		return h.refuseServerForm("your autodb user on that server is required", typed)
	}
	path, err := h.profilesPath()
	if err != nil {
		return h.refuseServerForm(err.Error(), typed)
	}
	profiles, err := remoteclient.LoadProfiles(path)
	if err != nil {
		return h.refuseServerForm(err.Error(), typed)
	}
	p := remoteclient.Profile{Name: strings.TrimSpace(name), Host: host, Port: port, User: user,
		KeyFile: keyFile, UseAgent: auth == "agent"}
	at := -1
	if h.serverFormID == "" {
		p.ID = newProfileID(p.Name, host, profiles)
	} else {
		for i := range profiles {
			if profiles[i].ID == h.serverFormID {
				at = i
			}
		}
		if at < 0 {
			return h.refuseServerForm("that server was removed meanwhile", typed)
		}
		p.ID, p.HostKeyFP = h.serverFormID, profiles[at].HostKeyFP
	}
	if err := p.Validate(); err != nil {
		return h.refuseServerForm(strings.TrimPrefix(err.Error(), remoteclient.ErrProfileInvalid.Error()+": "), typed)
	}
	if connected, ok := h.session.RemoteProfile(); ok && connected.ID == p.ID {
		return h.refuseServerForm("connected to "+profileLabel(p)+" — Disconnect first", typed)
	}
	if at < 0 {
		profiles = append(profiles, p)
	} else {
		old := profiles[at]
		profiles[at] = p
		// The device key is sealed to the SSH key it was enrolled with: with
		// another key file it can never be opened again.
		if old.KeyFile != p.KeyFile && h.deviceState(old) != "none yet" {
			seq := h.serverFormSeq
			h.confirm("use another SSH key for "+profileLabel(p)+"?",
				"This computer's device key for "+profileLabel(p)+" belongs to the SSH key "+old.KeyFile+
					", so it is deleted. The next connect with "+p.KeyFile+" enrolls this computer again, "+
					"as that key's device (the key must be registered on your profile there).",
				"&Change key", "&Keep", func() {
					if seq == h.serverFormSeq {
						h.writeServers(path, profiles, &p, "saved "+profileLabel(p))
					}
				})
			return nil
		}
	}
	h.writeServers(path, profiles, nil, "saved "+profileLabel(p))
	return nil
}

// writeServers saves profiles and, when discard names one, deletes its
// device key files; then the table again, saying done.
func (h *Host) writeServers(path string, profiles []remoteclient.Profile, discard *remoteclient.Profile, done string) {
	if err := remoteclient.SaveProfiles(path, profiles); err != nil {
		h.loadServers("could not save the remote servers: " + err.Error())
		return
	}
	if discard != nil {
		if err := h.discardDeviceKey(*discard); err != nil {
			done += ", but its device key could not be deleted: " + err.Error()
		}
	}
	h.loadServers(done)
}

// discardDeviceKey deletes this computer's device key files for p.
func (h *Host) discardDeviceKey(p remoteclient.Profile) error {
	k, err := h.keysFor(p)
	if err != nil {
		return err
	}
	return k.RemoveAll()
}

// serverRemove is App.serverRemove(i).
func (h *Host) serverRemove(i int) error {
	if !h.on(sectionServers) {
		return nil
	}
	p, ok := h.serverAt(i)
	if !ok {
		return nil
	}
	h.confirm("remove "+profileLabel(p)+"?",
		"This computer forgets "+profileLabel(p)+" and deletes its device key for it. If this computer's "+
			"device is still enrolled there, revoke it first under My devices while connected: otherwise "+
			"the server refuses this computer next time, as a different device with the same SSH key.",
		"Re&move", "&Keep", func() {
			path, err := h.profilesPath()
			if err != nil {
				h.set("App.manageStatus", err.Error())
				return
			}
			profiles, err := remoteclient.LoadProfiles(path)
			if err != nil {
				h.set("App.manageStatus", err.Error())
				return
			}
			kept := profiles[:0]
			for _, q := range profiles {
				if q.ID != p.ID {
					kept = append(kept, q)
				}
			}
			h.writeServers(path, kept, &p, "removed "+profileLabel(p))
		})
	return nil
}

// serverForget is App.serverForget(i): forget the pinned host key.
func (h *Host) serverForget(i int) error {
	if !h.on(sectionServers) {
		return nil
	}
	p, ok := h.serverAt(i)
	if !ok {
		return nil
	}
	if p.HostKeyFP == "" {
		h.set("App.manageStatus", "no host key is pinned for "+profileLabel(p))
		return nil
	}
	h.confirm("forget the host key of "+profileLabel(p)+"?",
		"Only when your admin says the server's host key changed. The next connect asks you to confirm "+
			"the new one. This computer's device key for "+profileLabel(p)+" is sealed to the old host key, "+
			"so it is deleted too, and an admin must revoke this computer's old device before it can "+
			"enroll again.",
		"&Forget", "&Keep", func() {
			path, err := h.profilesPath()
			if err != nil {
				h.set("App.manageStatus", err.Error())
				return
			}
			profiles, err := remoteclient.LoadProfiles(path)
			if err != nil {
				h.set("App.manageStatus", err.Error())
				return
			}
			for j := range profiles {
				if profiles[j].ID == p.ID {
					profiles[j].HostKeyFP = ""
				}
			}
			h.writeServers(path, profiles, &p, "forgot the host key of "+profileLabel(p))
		})
	return nil
}

// ---- My devices --------------------------------------------------------------

// myKeyAt is My devices row i.
func (h *Host) myKeyAt(i int) (SSHKeyRow, bool) {
	k, ok := h.mine.at(i)
	if !ok {
		h.set("App.manageStatus", "choose a key first")
	}
	return k, ok
}

// mineCurrent reports whether My devices' pinned identity is still the
// session's.
func (h *Host) mineCurrent(b *Bound) bool {
	return b != nil && b == h.mine.bound && b.Gen() == h.session.Gen() &&
		b.IdentityEpoch() == h.session.IdentityEpoch()
}

// keyAdd is App.keyAdd: the form to register a public key on one's own
// profile, with the passphrase.
func (h *Host) keyAdd() error {
	if !h.on(sectionMine) || h.mine.bound == nil {
		return nil
	}
	h.showKeyForm(0, "add an SSH key to your profile on "+h.connectedServer(), true, "")
	return nil
}

// keyLabel is App.keyLabel(i).
func (h *Host) keyLabel(i int) error {
	if !h.on(sectionMine) {
		return nil
	}
	if k, ok := h.myKeyAt(i); ok {
		h.showKeyForm(k.ID, "label the key "+shortFP(k.Fingerprint), false, k.Label)
	}
	return nil
}

func (h *Host) showKeyForm(keyID int64, title string, adding bool, label string) {
	h.keyFormBound, h.keyFormID = h.mine.bound, keyID
	h.refill(map[string]any{"App.keyFormTitle": title, "App.keyFormError": "",
		"App.keyFormLabel": label, "App.keyFormPublic": ""})
	h.set("App.keyFormAdding", adding)
	h.open("sshKeyForm")
}

// keyFormClosed is App.keyFormClosed: the form cancelled.
func (h *Host) keyFormClosed() error {
	h.keyFormBound = nil
	return nil
}

// refuseKeyForm opens the key form again with what was typed (never the
// passphrase), saying why.
func (h *Host) refuseKeyForm(why, public, label string) {
	if err := h.p.SetMany(map[string]any{"App.keyFormError": why,
		"App.keyFormPublic": public, "App.keyFormLabel": label}); err != nil {
		h.keep(err)
	}
	h.p.Post(func() { h.open("sshKeyForm") })
}

// saveKey is App.saveKey(public, label, passphrase): the key form answered.
func (h *Host) saveKey(public, label, passphrase string) error {
	b := h.keyFormBound
	if !h.mineCurrent(b) {
		return nil
	}
	adding := h.keyFormID == 0
	if adding {
		public = strings.TrimSpace(public)
		if public == "" {
			h.refuseKeyForm("paste the public key: one line of your .pub file", public, label)
			return nil
		}
		if passphrase == "" {
			h.refuseKeyForm("your autodb passphrase is required to add a key", public, label)
			return nil
		}
	}
	keyID := h.keyFormID
	do(h, func(ctx context.Context) error {
		if adding {
			_, err := b.AddSSHKey(ctx, 0, public, label, passphrase)
			return err
		}
		return b.LabelSSHKey(ctx, keyID, label)
	}, func(err error) {
		if !h.mineCurrent(b) {
			return // signed out (a wrong passphrase does that), or moved on
		}
		if err != nil {
			h.refuseKeyForm(WireErrorMessage(err), public, label)
			return
		}
		h.keyFormBound = nil
		done := "labelled"
		if adding {
			done = "added the key — the first connect with it enrolls that computer as its device"
		}
		reloadManager(h, h.mine, done)
	})
	return nil
}

// keyRevoke is App.keyRevoke(i): revoke an SSH key, its device with it.
func (h *Host) keyRevoke(i int) error {
	if !h.on(sectionMine) {
		return nil
	}
	k, ok := h.myKeyAt(i)
	if !ok {
		return nil
	}
	self := h.connectedWith(k.Fingerprint)
	text := "The key can no longer connect, and its device is revoked with it. This cannot be undone: " +
		"to use the key again, register it again."
	if self {
		text += "\n\nThis computer is connected with this key: this session ends, and you are back on the local daemon."
	}
	h.confirm("revoke the SSH key "+shortFP(k.Fingerprint)+"?", text, "&Revoke", "&Keep", func() {
		h.revokeMine("revoke the key", self, func(ctx context.Context, b *Bound) error {
			return b.RevokeSSHKey(ctx, k.ID)
		})
	})
	return nil
}

// deviceRevoke is App.deviceRevoke(i): revoke the device a key is bound to.
func (h *Host) deviceRevoke(i int) error {
	if !h.on(sectionMine) {
		return nil
	}
	k, ok := h.myKeyAt(i)
	if !ok {
		return nil
	}
	if k.Device == nil {
		h.set("App.manageStatus", "that key has no device yet")
		return nil
	}
	self := h.connectedWith(k.Fingerprint)
	text := "The computer enrolled with this key can no longer connect. The key stays registered, so the " +
		"next computer to connect with it enrolls as its device."
	if self {
		text += "\n\nIt is this computer: this session ends, you are back on the local daemon, and this " +
			"computer's device key for the server is deleted."
	}
	dev := k.Device.ID
	h.confirm("revoke the device of "+shortFP(k.Fingerprint)+"?", text, "&Revoke", "&Keep", func() {
		h.revokeMine("revoke the device", self, func(ctx context.Context, b *Bound) error {
			return b.RevokeDevice(ctx, dev)
		})
	})
	return nil
}

// revokeMine runs a revocation from My devices. One that ends this
// computer's own connection (self) holds the connection's watcher off first:
// reconnecting would present a device the server has just refused. Answered,
// the device key is deleted and the session goes back to the local daemon;
// with no answer (the connection ended first) nothing is known, so the key
// is kept.
func (h *Host) revokeMine(what string, self bool, fn func(context.Context, *Bound) error) {
	if !self {
		managerCall(h, h.mine, what, fn)
		return
	}
	b := h.mine.bound
	profile, remote := h.session.RemoteProfile()
	if !remote || !h.mineCurrent(b) {
		h.set("App.manageStatus", what+": the connection changed — nothing was done here")
		return
	}
	h.leavingGen = h.session.Gen()
	held := h.selfRevokeAnswered
	do(h, func(ctx context.Context) error {
		err := fn(ctx, b)
		if held != nil {
			held()
		}
		return err
	}, func(err error) {
		if b.Gen() != h.session.Gen() {
			return // switched away meanwhile
		}
		var re *golibrpc.Error
		if err != nil && errors.As(err, &re) {
			// Refused, and answered: the connection is as it was, unless the
			// refusal was of the sign-in itself.
			h.leavingGen = 0
			h.set("App.manageStatus", what+": "+WireErrorMessage(err))
			if !h.session.Connected() {
				h.connect()
				return
			}
			h.checkAuth()
			return
		}
		status := "revoked this computer's device on " + profileLabel(profile) + " — back on the local daemon"
		if err != nil {
			status = "the connection to " + profileLabel(profile) + " ended before it answered (" + err.Error() +
				") — back on the local daemon; if the revocation went through, remove the server under Remote › Manage"
		} else if derr := h.discardDeviceKey(profile); derr != nil {
			status += ", but its device key could not be deleted: " + derr.Error()
		}
		h.backToLocal(status)
	})
}

// leavingRemote reports whether the connection in use is being given up by
// a revocation of this computer's own device: its watcher and a lost sign-in
// must not reconnect it.
func (h *Host) leavingRemote() bool {
	return h.leavingGen != 0 && h.leavingGen == h.session.Gen()
}
