package tui

import (
	"context"
	"errors"
	"fmt"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"

	"github.com/yongjohnlee80/autodb/core/remoteclient"
	"github.com/yongjohnlee80/autodb/tui/remotedial"
)

// The Remote menu: reaching a remote autodb server over its SSH listener.
//
// Connect and Disconnect are identity changes, not reconnects (a remote
// session takes nothing from the client side). Each runs, in order: the
// unsaved-note guard; the retirement of the current identity (its notes, its
// workspace, results, history and search); the session's move to the other
// transport, which forgets the token and moves the generation and the
// identity epoch; and then a sign-in on the new side, after which the
// catalog is projected from the new identity's role. Nothing carries across.

// firstConnectWarning is the first-connect dialog's warning: the passphrase
// can only be checked by the server there, so a typo is a counted refusal.
const firstConnectWarning = "This is your autodb passphrase. A wrong entry counts as a failed attempt; " +
	"three in a row block this network for 24 hours."

// remoteState is the Remote menu's and the Connect dialog's sources.
func remoteMenuState(h *Host) map[string]any {
	return map[string]any{
		"App.remoteProfiles":     h.remoteProfiles,
		"App.remoteProfile":      -1,
		"App.remoteFirst":        false,
		"App.remoteWarning":      "",
		"App.remoteConnectError": "",
		"App.keyPassphrasePath":  "",
	}
}

// profilesPath and keyDir are where the remote profiles and device keys are:
// the options' paths, or the defaults.
func (h *Host) profilesPath() (string, error) {
	if h.remotePaths.profiles != "" {
		return h.remotePaths.profiles, nil
	}
	return remoteclient.ProfilesPath()
}

func (h *Host) keyDir() (string, error) {
	if h.remotePaths.keys != "" {
		return h.remotePaths.keys, nil
	}
	return remoteclient.DeviceKeyDir()
}

// remoteConnected is whether the session is a remote one that is signed in.
func (h *Host) remoteConnected() bool { return h.session.Remote() && h.session.Token() != "" }

// remoteProfileName is the remote session's profile, for the menu.
func (h *Host) remoteProfileName() string {
	p, _ := h.session.RemoteProfile()
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}

// openRemoteConnect is remote.connect: after the unsaved-note guard, the
// Connect dialog over the profiles in remotes.toml.
func (h *Host) openRemoteConnect() { h.openRemoteConnectAt("") }

// openRemoteConnectAt is the Connect dialog at profile id ("" the first).
func (h *Host) openRemoteConnectAt(id string) {
	h.guardUnsaved(func() {
		path, err := h.profilesPath()
		if err != nil {
			h.setStatus("remote profiles: " + err.Error())
			return
		}
		profiles, err := remoteclient.LoadProfiles(path)
		if err != nil {
			h.setStatus("remote profiles: " + err.Error())
			return
		}
		if len(profiles) == 0 {
			h.statusMessage("autodb.remote_menu.status.status")
			return
		}
		h.profiles = profiles
		rows := make([]tuidecl.Row, 0, len(profiles))
		for _, p := range profiles {
			label := p.Name
			if label == "" {
				label = p.ID
			}
			rows = append(rows, tuidecl.Row{"key": p.ID, "name": fmt.Sprintf("%s — %s as %s", label, p.Address(), p.User)})
		}
		h.remoteProfiles.Reset(rows)
		at := 0
		for i, p := range profiles {
			if p.ID == id {
				at = i
			}
		}
		if id != "" && profiles[at].ID != id {
			h.setStatus("no remote server " + id + " in " + path)
		}
		h.set("App.remoteProfile", -1)
		h.set("App.remoteProfile", at)
		h.set("App.remoteConnectError", "")
		h.remoteChosen(at)
		h.open("remoteConnect")
	})
}

// remoteChosen is App.remoteChoose(row): the dialog follows the chosen
// profile, asking the passphrase twice, with the warning, on a first connect
// from this machine.
func (h *Host) remoteChosen(row int) error {
	first := false
	if row >= 0 && row < len(h.profiles) {
		first = h.firstConnect(h.profiles[row])
	}
	warning := ""
	if first {
		warning = firstConnectWarning
	}
	h.set("App.remoteFirst", first)
	h.set("App.remoteWarning", warning)
	return nil
}

// firstConnect is whether no device key for p is on this machine yet.
func (h *Host) firstConnect(p remoteclient.Profile) bool {
	dir, err := h.keyDir()
	if err != nil {
		return true
	}
	k, err := remoteclient.FilesFor(dir, p.ID)
	if err != nil {
		return true
	}
	return !k.Exists(k.Key()) && !k.Exists(k.Pending()) && !k.Exists(k.Next())
}

// remoteConnectAnswered is App.remoteConnect(profile, passphrase, again): the
// dialog answered. The session leaves its current transport here, and
// connects to the remote.
func (h *Host) remoteConnectAnswered(id, passphrase, again string) error {
	refuse := func(why string) error {
		h.set("App.remoteConnectError", why)
		h.p.Post(func() { h.open("remoteConnect") })
		return nil
	}
	var profile remoteclient.Profile
	found := false
	for _, p := range h.profiles {
		if p.ID == id {
			profile, found = p, true
		}
	}
	if !found {
		return refuse("choose a server")
	}
	if passphrase == "" {
		return refuse("enter your autodb passphrase for " + profile.User)
	}
	first := h.firstConnect(profile)
	if first && passphrase != again {
		return refuse("the two entries differ — type the passphrase again")
	}
	dir, err := h.keyDir()
	if err != nil {
		return refuse(err.Error())
	}
	keys, err := remoteclient.FilesFor(dir, profile.ID)
	if err != nil {
		return refuse(err.Error())
	}
	d := &remotedial.Dialer{Profile: profile, Keys: keys, ClientVersion: h.about.Version,
		ConfirmHostKey: h.askHostKey, KeyPassphrase: h.askKeyPassphrase(profile.KeyFile)}

	// The switch: the current identity ends before the next begins.
	h.hadAuth = false
	h.retireIdentity()
	h.session.SwitchToRemote(d)
	h.connecting = true
	h.setAuth("connecting")
	h.setStatus(fmt.Sprintf("connecting to %s as %s…", profile.Address(), profile.User))
	h.refreshIdentity()
	type result struct {
		hostFP string
		err    error
	}
	sess := h.session
	do(h, func(ctx context.Context) result {
		fp, err := sess.ConnectRemote(ctx, passphrase)
		return result{fp, err}
	}, func(r result) {
		h.connecting = false
		if h.session.RemoteDialer() != d {
			return // cancelled, or another connect since
		}
		if r.err != nil {
			h.setAuth("disconnected")
			h.refreshIdentity()
			refuse("could not connect: " + r.err.Error())
			return
		}
		if profile.HostKeyFP == "" && r.hostFP != "" {
			if err := h.pinHostKey(profile.ID, r.hostFP); err != nil {
				h.setStatus("connected, but the host key could not be pinned: " + err.Error())
			}
		}
		h.connectedOnce = true
		h.awaitingStart = false
		h.watch(h.session.Gen())
		h.afterSignIn()
		h.setStatus(fmt.Sprintf("connected to %s as %s (%s)", profile.Address(), h.session.User().Name, h.session.User().Role))
	})
	return nil
}

// remoteConnectCancelled is App.remoteConnectCancelled: the dialog closed
// without connecting. A session that left for the remote and did not get
// there goes back to the local daemon.
func (h *Host) remoteConnectCancelled() error {
	if h.connecting {
		return nil
	}
	if h.awaitingStart {
		// The program started for this connect: nothing else is dialed in
		// its place.
		h.awaitingStart = false
		if h.session.Remote() {
			h.hadAuth = false
			h.retireIdentity()
			h.session.SwitchToLocal()
		}
		h.setAuth("disconnected")
		h.setStatus(notStartedHint)
		h.refreshIdentity()
		return nil
	}
	if h.session.Remote() && h.session.Token() == "" {
		h.backToLocal("not connected to the remote server — back to the local daemon")
	}
	return nil
}

// askHostKey asks, on a first connect, whether the server's host key is the
// admin's. It is called from the dial, off the UI loop, and waits for the
// answer.
func (h *Host) askHostKey(fingerprint string) bool {
	answer := make(chan bool, 1)
	h.p.Post(func() {
		h.confirmOr("confirm the server's host key",
			"This is the first connect to this server. Check this fingerprint against the one your admin shared:\n\n"+
				fingerprint+"\n\nTrust it? From now on a different key is refused.",
			"&Trust", "&Cancel",
			func() { answer <- true }, func() { answer <- false })
	})
	select {
	case v := <-answer:
		return v
	case <-h.ctx.Done():
		return false
	}
}

// errKeyPassphraseNotGiven: the SSH key's passphrase question was closed
// unanswered.
var errKeyPassphraseNotGiven = errors.New("the ssh key's passphrase was not given")

// askKeyPassphrase is the dialer's question for the SSH key file's own
// passphrase. Like askHostKey it is called off the UI loop, and waits for the
// answer. The answer goes to the dialer only: the key it opens is kept in
// memory for that Connect, and the passphrase itself is kept nowhere.
func (h *Host) askKeyPassphrase(path string) func() (string, error) {
	return func() (string, error) {
		answer := make(chan keyPassphraseAnswer, 1)
		h.p.Post(func() {
			h.keyPassphrase = answer
			h.set("App.keyPassphrasePath", path)
			h.open("keyPassphrase")
		})
		select {
		case a := <-answer:
			return a.pass, a.err
		case <-h.ctx.Done():
			return "", errKeyPassphraseNotGiven
		}
	}
}

type keyPassphraseAnswer struct {
	pass string
	err  error
}

// keyPassphraseAnswered is App.keyPassphraseAnswered(passphrase).
func (h *Host) keyPassphraseAnswered(passphrase string) error {
	if a := h.keyPassphrase; a != nil {
		h.keyPassphrase = nil
		a <- keyPassphraseAnswer{pass: passphrase}
	}
	return nil
}

// keyPassphraseCancelled is App.keyPassphraseCancelled: the question closed
// unanswered.
func (h *Host) keyPassphraseCancelled() error {
	if a := h.keyPassphrase; a != nil {
		h.keyPassphrase = nil
		a <- keyPassphraseAnswer{err: errKeyPassphraseNotGiven}
	}
	return nil
}

// pinHostKey records fp as profile id's host key in remotes.toml.
func (h *Host) pinHostKey(id, fp string) error {
	path, err := h.profilesPath()
	if err != nil {
		return err
	}
	profiles, err := remoteclient.LoadProfiles(path)
	if err != nil {
		return err
	}
	for i := range profiles {
		if profiles[i].ID == id {
			profiles[i].HostKeyFP = fp
		}
	}
	return remoteclient.SaveProfiles(path, profiles)
}

// remoteDisconnect is remote.disconnect: after the unsaved-note guard, the
// remote session is signed out and closed, the device key wiped, and the
// session goes back to the local daemon, which needs a local sign-in.
func (h *Host) remoteDisconnect() {
	if !h.session.Remote() {
		return
	}
	h.guardUnsaved(func() {
		b := h.session.Bind()
		signedIn := h.session.Token() != ""
		do(h, func(ctx context.Context) struct{} {
			if signedIn {
				_ = b.Logout(ctx) // the session is revoked, not left to resume
			}
			return struct{}{}
		}, func(struct{}) {
			h.backToLocal("disconnected from the remote server")
		})
	})
}

// backToLocal ends the remote identity and connects the local daemon.
func (h *Host) backToLocal(status string) {
	h.hadAuth = false
	h.retireIdentity()
	h.session.SwitchToLocal()
	h.clearFrontDoorWarning()
	h.setAuth("disconnected")
	h.setStatus(status)
	h.refreshIdentity()
	h.connect()
}
