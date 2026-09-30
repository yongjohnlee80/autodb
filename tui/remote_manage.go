package tui

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

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
//   - Remote activity is the connected server's enrollments, new addresses
//     and refusals: an admin's all of them, anyone else's their own
//     enrollments and new addresses.
//   - Remote Control, Devices (every user's keys and devices) and Blocked IPs
//     are an admin's, on the connected server.
//   - SSH keys of a user is opened from System › Users, for an admin.
//
// A profile the session is connected to is not changed under it: Disconnect
// first. Whatever ends this computer's own remote connection — revoking the
// key or device it is connected with, turning Remote Control off from a
// remote session — goes back to the local daemon instead of reconnecting
// (remote_keys.go, leaveRemote).

const (
	sectionServers  = "servers"
	sectionMine     = "mine"
	sectionActivity = "activity"
	sectionControl  = "control"
	sectionDevices  = "devices"
	sectionBlocks   = "blocks"
	sectionUserKeys = "userkeys"
)

type manageSection struct{ id, label string }

func manageState(h *Host) map[string]any {
	return map[string]any{
		"App.manageSections":     h.manageSectionModel,
		"App.manageSectionIndex": -1,
		"App.manageStatus":       "",
		"App.manageOnServers":    false,
		"App.manageOnKeys":       false,
		"App.manageKeysEdit":     false,
		"App.manageKeysTitle":    "",
		"App.manageOnControl":    false,
		"App.manageOnBlocks":     false,
		"App.manageOnActivity":   false,
		"App.serverRows":         h.serverRows,
		"App.keyRows":            h.keys.model,
		"App.blockRows":          h.blocks.model,
		"App.activityRows":       h.activity.model,
		"App.activityKinds":      h.activityKinds,
		"App.activityKindIndex":  -1,
		"App.controlText":        "",
		"App.controlToggleLabel": "&Turn on",
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
		"App.keyFormPassphrase":  false,
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

// connectedServer is the server the session is on: the remote profile, or
// the local daemon.
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
	if h.session.Token() == "" {
		return out
	}
	where := h.connectedServer()
	out = append(out, manageSection{sectionMine, "My devices — your SSH keys on " + where})
	admin := h.session.IsAdmin()
	if admin {
		out = append(out, manageSection{sectionActivity, "Remote activity — enrollments, new addresses and refusals on " + where})
	} else {
		out = append(out, manageSection{sectionActivity, "My remote activity — your enrollments and new addresses on " + where})
	}
	if !admin {
		return out
	}
	out = append(out,
		manageSection{sectionControl, "Remote Control — " + where + "'s remote listener"},
		manageSection{sectionDevices, "Devices — every user's SSH keys and devices on " + where},
		manageSection{sectionBlocks, "Blocked IPs — addresses refused on " + where},
	)
	if h.userKeysFor.id != 0 {
		out = append(out, manageSection{sectionUserKeys, "SSH keys of " + h.userKeysFor.name + " on " + where})
	}
	return out
}

// openManage is remote.manage (Home › My SSH keys… at sectionMine, System ›
// Users › SSH keys at sectionUserKeys): the dialog, at section when it is
// offered, else at the first.
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

// isKeys reports whether section is one of the SSH key tables.
func isKeys(section string) bool {
	return section == sectionMine || section == sectionDevices || section == sectionUserKeys
}

// showSection shows section i, loading it.
func (h *Host) showSection(i int) {
	h.section = h.sections[i].id
	if err := h.p.SetMany(map[string]any{
		"App.manageOnServers":  h.section == sectionServers,
		"App.manageOnKeys":     isKeys(h.section),
		"App.manageKeysEdit":   h.section == sectionMine || h.section == sectionUserKeys,
		"App.manageOnControl":  h.section == sectionControl,
		"App.manageOnBlocks":   h.section == sectionBlocks,
		"App.manageOnActivity": h.section == sectionActivity,
		"App.manageStatus":     "",
	}); err != nil {
		h.keep(err)
		return
	}
	switch {
	case h.section == sectionServers:
		h.loadServers("")
	case isKeys(h.section):
		h.openKeys()
	case h.section == sectionControl:
		h.loadControl("")
	case h.section == sectionBlocks:
		h.blocks.all, h.blocks.rows = nil, nil
		h.blocks.model.Reset(nil)
		openManager(h, h.blocks)
	case h.section == sectionActivity:
		h.openActivity()
	}
}

// on reports whether the dialog is at section: a button of another section
// (hidden, but its key still reaches the dialog) does nothing.
func (h *Host) on(section string) bool { return h.section == section }

// manageClosed is App.manageClosed.
func (h *Host) manageClosed() error {
	h.section = ""
	h.keys.bound, h.blocks.bound, h.activity.bound, h.controlBound = nil, nil, nil, nil
	h.userKeysFor.id, h.userKeysFor.name = 0, ""
	h.serverFormSeq++
	h.keyFormBound = nil
	return nil
}

// manageRefresh is App.manageRefresh: the section again.
func (h *Host) manageRefresh() error {
	switch {
	case h.section == sectionServers:
		h.loadServers("")
	case isKeys(h.section) && h.keys.bound != nil:
		reloadManager(h, h.keys, "")
	case h.section == sectionControl:
		h.loadControl("")
	case h.section == sectionBlocks && h.blocks.bound != nil:
		reloadManager(h, h.blocks, "")
	case h.section == sectionActivity && h.activity.bound != nil:
		h.activityReload()
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
