package tui

import (
	"context"
	"errors"
	"strconv"
	"strings"

	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
)

// The Manage dialog's SSH key tables — My devices (the caller's own), Devices
// (everyone's, for an admin) and SSH keys of a user (from System › Users) —
// are one table over the connected server's keys, each row with its device.
// They differ in whose keys they list, and in that Devices only revokes:
// keys are added and labelled on a profile, one's own or, for an admin, the
// user's.

// newKeysManager lists the keys of h.keysUser: 0 the caller's, -1
// everyone's, else that user's.
func newKeysManager(h *Host) *manager[SSHKeyRow] {
	m := newManager("App.manageStatus",
		func(context.Context, *Bound) ([]SSHKeyRow, error) { return nil, nil }, // set per open
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
			return tuidecl.Row{"key": strconv.FormatInt(k.ID, 10), "user": k.User, "label": label,
				"fingerprint": shortFP(k.Fingerprint), "type": k.Type, "device": device,
				"lastIP": lastIP, "lastSeen": lastSeen}
		}, "key", "user", "label", "fingerprint", "type", "device", "lastIP", "lastSeen")
	return m
}

// connectedWith reports whether the remote connection in use authenticated
// with the SSH key of fingerprint fp: that key's device is this computer.
func (h *Host) connectedWith(fp string) bool {
	own := h.session.RemoteSSHKeyFP()
	return own != "" && own == fp
}

// openKeys loads the key table for the section shown.
func (h *Host) openKeys() {
	where := h.connectedServer()
	var title string
	switch h.section {
	case sectionMine:
		h.keysUser = 0
		title = "your SSH keys on " + where + ", and the device each is bound to"
	case sectionDevices:
		h.keysUser = -1
		title = "every user's SSH keys on " + where + ", and the device each is bound to"
	case sectionUserKeys:
		h.keysUser = h.userKeysFor.id
		title = h.userKeysFor.name + "'s SSH keys on " + where + ", and the device each is bound to"
	}
	user := h.keysUser
	h.keys.load = func(ctx context.Context, b *Bound) ([]SSHKeyRow, error) { return b.SSHKeys(ctx, user) }
	h.set("App.manageKeysTitle", title)
	h.keys.all, h.keys.rows = nil, nil
	h.keys.model.Reset(nil)
	openManager(h, h.keys)
}

// keyAt is the key table's row i.
func (h *Host) keyAt(i int) (SSHKeyRow, bool) {
	k, ok := h.keys.at(i)
	if !ok {
		h.set("App.manageStatus", "choose a key first")
	}
	return k, ok
}

// keysCurrent reports whether the key table's pinned identity is still the
// session's.
func (h *Host) keysCurrent(b *Bound) bool {
	return b != nil && b == h.keys.bound && b.Gen() == h.session.Gen() &&
		b.IdentityEpoch() == h.session.IdentityEpoch()
}

// keysEditable reports whether the section shown adds and labels keys.
func (h *Host) keysEditable() bool {
	return (h.on(sectionMine) || h.on(sectionUserKeys)) && h.keys.bound != nil
}

// keyAdd is App.keyAdd: the form to register a public key. On one's own
// profile it asks for the passphrase; an admin adding to another's does not.
func (h *Host) keyAdd() error {
	if !h.keysEditable() {
		return nil
	}
	title := "add an SSH key to your profile on " + h.connectedServer()
	if h.on(sectionUserKeys) {
		title = "add an SSH key to " + h.userKeysFor.name + "'s profile on " + h.connectedServer()
	}
	h.showKeyForm(0, title, true, "")
	return nil
}

// keyLabel is App.keyLabel(i).
func (h *Host) keyLabel(i int) error {
	if !h.keysEditable() {
		return nil
	}
	if k, ok := h.keyAt(i); ok {
		h.showKeyForm(k.ID, "label the key "+shortFP(k.Fingerprint), false, k.Label)
	}
	return nil
}

// ownProfile reports whether the key table is the caller's own keys.
func (h *Host) ownProfile(b *Bound) bool {
	return h.keysUser == 0 || h.keysUser == b.User().ID
}

func (h *Host) showKeyForm(keyID int64, title string, adding bool, label string) {
	h.keyFormBound, h.keyFormID = h.keys.bound, keyID
	h.refill(map[string]any{"App.keyFormTitle": title, "App.keyFormError": "",
		"App.keyFormLabel": label, "App.keyFormPublic": ""})
	h.set("App.keyFormAdding", adding)
	h.set("App.keyFormPassphrase", adding && h.ownProfile(h.keys.bound))
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
	if !h.keysCurrent(b) {
		return nil
	}
	adding := h.keyFormID == 0
	own := h.ownProfile(b)
	if adding {
		public = strings.TrimSpace(public)
		if public == "" {
			h.refuseKeyForm("paste the public key: one line of the .pub file", public, label)
			return nil
		}
		if own && passphrase == "" {
			h.refuseKeyForm("your autodb passphrase is required to add a key", public, label)
			return nil
		}
		if !own {
			passphrase = ""
		}
	}
	keyID, user := h.keyFormID, h.keysUser
	do(h, func(ctx context.Context) error {
		if adding {
			_, err := b.AddSSHKey(ctx, user, public, label, passphrase)
			return err
		}
		return b.LabelSSHKey(ctx, keyID, label)
	}, func(err error) {
		if !h.keysCurrent(b) {
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
		reloadManager(h, h.keys, done)
	})
	return nil
}

// keyRevoke is App.keyRevoke(i): revoke an SSH key, its device with it.
func (h *Host) keyRevoke(i int) error {
	if !isKeys(h.section) {
		return nil
	}
	k, ok := h.keyAt(i)
	if !ok {
		return nil
	}
	self := h.connectedWith(k.Fingerprint)
	text := "The key can no longer connect, and its device is revoked with it. This cannot be undone: " +
		"to use the key again, register it again."
	if self {
		text += "\n\nThis computer is connected with this key: this session ends, and you are back on the local daemon."
	}
	h.confirm("revoke the SSH key "+shortFP(k.Fingerprint)+" of "+k.User+"?", text, "&Revoke", "&Keep", func() {
		h.revokeKey("revoke the key", self, func(ctx context.Context, b *Bound) error {
			return b.RevokeSSHKey(ctx, k.ID)
		})
	})
	return nil
}

// deviceRevoke is App.deviceRevoke(i): revoke the device a key is bound to.
func (h *Host) deviceRevoke(i int) error {
	if !isKeys(h.section) {
		return nil
	}
	k, ok := h.keyAt(i)
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
	h.confirm("revoke the device of "+shortFP(k.Fingerprint)+" ("+k.User+")?", text, "&Revoke", "&Keep", func() {
		h.revokeKey("revoke the device", self, func(ctx context.Context, b *Bound) error {
			return b.RevokeDevice(ctx, dev)
		})
	})
	return nil
}

// revokeKey runs a revocation from a key table. One of this computer's own
// key or device (self) ends this connection: leaveRemote. Answered, the
// device key is deleted; with no answer nothing is known, so it is kept.
func (h *Host) revokeKey(what string, self bool, fn func(context.Context, *Bound) error) {
	if !self {
		managerCall(h, h.keys, what, fn)
		return
	}
	profile, _ := h.session.RemoteProfile()
	h.leaveRemote(h.keys.bound, what, fn, func(err error) string {
		if err != nil {
			return "the connection to " + profileLabel(profile) + " ended before it answered (" + err.Error() +
				") — back on the local daemon; if the revocation went through, remove the server under Remote › Manage"
		}
		status := "revoked this computer's device on " + profileLabel(profile) + " — back on the local daemon"
		if derr := h.discardDeviceKey(profile); derr != nil {
			status += ", but its device key could not be deleted: " + derr.Error()
		}
		return status
	})
}

// leaveRemote runs fn, which ends this computer's own remote connection, on
// b. The connection's watcher and a lost sign-in are held off first:
// reconnecting would present a device the server may have just refused, or
// meet a listener that is gone. Refused (an answer that is an error), the
// connection is as it was. Otherwise — answered, or the connection ended
// first — the session goes back to the local daemon, with the status left
// makes of the outcome (nil: answered).
func (h *Host) leaveRemote(b *Bound, what string, fn func(context.Context, *Bound) error, left func(error) string) {
	if !h.session.Remote() || b == nil || b.Gen() != h.session.Gen() || b.IdentityEpoch() != h.session.IdentityEpoch() {
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
		h.backToLocal(left(err))
	})
}

// leavingRemote reports whether the connection in use is being given up
// (leaveRemote): its watcher and a lost sign-in must not reconnect it.
func (h *Host) leavingRemote() bool {
	return h.leavingGen != 0 && h.leavingGen == h.session.Gen()
}

// tellNewDevices is the one-time notice of a remote sign-in: each of the
// user's devices enrolled since this one last signed in, in turn, with the
// choice to revoke it. Revoking it revokes its SSH key with it: a device
// revoked alone leaves its key free to enroll again, and a device that was
// not the user's means that key was taken. It is never this computer's key,
// which has this device.
func (h *Host) tellNewDevices() {
	told := h.session.TakeNewDevices()
	if len(told) == 0 {
		return
	}
	b := h.session.Bind()
	var next func(i int)
	next = func(i int) {
		if i >= len(told) || b.Gen() != h.session.Gen() || b.IdentityEpoch() != h.session.IdentityEpoch() {
			return
		}
		d := told[i]
		h.confirmOr("a new device signed in to your account",
			"New device enrolled from "+d.EnrolledIP+" on "+shortDate(d.EnrolledAt)+" — not you? Revoke it.\n\n"+
				"Its device fingerprint: "+d.Fingerprint+"\n\n"+
				"Revoking it also revokes the SSH key it connected with, so the key cannot enroll another device. "+
				"If it was not you, change your autodb passphrase too: it was used.",
			"&Revoke it", "&It was me",
			func() {
				do(h, func(ctx context.Context) error { return b.RevokeSSHKey(ctx, d.SSHKeyID) }, func(err error) {
					if err != nil {
						h.setStatus("revoking the device enrolled from " + d.EnrolledIP + ": " + WireErrorMessage(err))
					} else {
						h.setStatus("revoked the device enrolled from " + d.EnrolledIP + " on " +
							shortDate(d.EnrolledAt) + ", and its SSH key")
					}
					next(i + 1)
				})
			},
			func() { next(i + 1) })
	}
	next(0)
}

// userSSHKeys is App.userSSHKeys(i): System › Users › SSH keys, the chosen
// user's keys in the Manage dialog.
func (h *Host) userSSHKeys(i int) error {
	if !h.userWritable() {
		return nil
	}
	u, ok := h.selectedUser(i)
	if !ok {
		return nil
	}
	h.userKeysFor.id, h.userKeysFor.name = u.ID, u.Name
	h.openManage(sectionUserKeys)
	return nil
}
