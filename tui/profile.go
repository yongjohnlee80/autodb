package tui

import (
	"context"
	"strings"
)

// Profile is the signed-in caller's own account, never an admin-selected
// user. The current passphrase unwraps the owner's key at the RPC boundary;
// it is never published as an App source or included in an error message.
func (h *Host) openProfile() {
	b := h.session.Bind()
	if b.User().Name == "" {
		h.setStatus("profile: sign in first")
		return
	}
	h.profileBound = b
	h.set("App.profileError", "")
	h.set("App.profileText", strings.Join([]string{
		"signed in as   " + b.User().Name,
		"role           " + b.User().Role,
		"", "Changing your own passphrase requires the current one.",
		"An administrator reset is a different operation.",
	}, "\n"))
	h.open("profile")
}

func (h *Host) profileClosed() error {
	h.profileBound = nil
	return nil
}

func (h *Host) changePassphrase(oldPass, next, again string) error {
	b := h.profileBound
	if b == nil || b.Gen() != h.session.Gen() || b.IdentityEpoch() != h.session.IdentityEpoch() {
		h.set("App.profileError", "sign-in changed; open your profile again")
		return nil
	}
	switch {
	case h.profilePending:
		h.set("App.profileError", "a passphrase change is already running")
	case oldPass == "" || next == "" || again == "":
		h.set("App.profileError", "all three passphrase fields are required")
	case next != again:
		h.set("App.profileError", "the two new passphrases do not match")
	case oldPass == next:
		h.set("App.profileError", "the new passphrase must differ from the current one")
	default:
		h.profilePending = true
		h.set("App.profileError", "changing passphrase…")
		do(h, func(ctx context.Context) error { return b.ChangePassphrase(ctx, oldPass, next) }, func(err error) {
			h.profilePending = false
			if b.Gen() != h.session.Gen() || b.IdentityEpoch() != h.session.IdentityEpoch() {
				return
			}
			if err != nil {
				h.set("App.profileError", "change failed: "+WireErrorMessage(err))
				return
			}
			h.setStatus("passphrase changed")
			h.profileBound = nil
			if err := h.p.Call("profile", "close"); err != nil {
				h.keep(err)
			}
		})
	}
	return nil
}
