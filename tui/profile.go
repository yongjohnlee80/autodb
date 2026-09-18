package tui

// PROFILE: what this session's account is, and the one thing its owner can
// change about it without an administrator.
//
// Everything an operator could previously learn about their own account, they
// learned from the status bar (a name) or from the Users manager (which an
// editor cannot open at all). Changing their own passphrase had no surface
// whatsoever: the daemon has served auth.passphrase_change since the auth
// service was written, and nothing in the frontend called it. The only route
// was to ask an administrator for a reset, which is a DIFFERENT operation with
// a different security story -- the service unwraps with its own copy of the
// master key rather than with the passphrase the owner still knows.

import (
	"context"
	"errors"
	"strings"
)

// openProfile shows the signed-in account and offers the self-service change.
func (m *Model) openProfile() {
	u := m.session.User()
	if u.Name == "" {
		m.setError("profile: nobody is signed in")
		return
	}
	NewConfirmModal(m, "profile",
		strings.Join([]string{
			"signed in as   " + u.Name,
			"role           " + u.Role,
			"",
			"Change passphrase replaces your own passphrase. You need the",
			"current one: your account's master key is sealed under it, so",
			"the daemon unwraps with the old and re-wraps with the new.",
			"An administrator's reset is a different operation.",
		}, "\n")).
		WithOkText("Change passphrase").
		WithCancelText("Close").
		WithSubmitFn(func(ModalResponse) error {
			m.openChangePassphrase()
			return nil
		}).Open()
}

// ErrPassphraseMismatch is what the confirmation row refuses with.
var ErrPassphraseMismatch = errors.New("the two entries do not match")

// openChangePassphrase asks for the old passphrase and the new one twice.
//
// THE CONFIRMATION ROW IS NOT CEREMONY. The field is masked, so a typo is
// invisible; and the failure it produces is not "the new passphrase is wrong"
// but "you can no longer sign in", discovered at the next login with nothing
// left to compare against. Validated HERE rather than at the daemon, because
// the daemon is told one new passphrase and has nothing to check it against.
func (m *Model) openChangePassphrase() {
	var oldPass, newPass, confirm string
	NewInputModal(m, "change passphrase",
		NewTextInput("current passphrase", &oldPass).Masked().Required(),
		NewTextInput("new passphrase", &newPass).Masked().Required(),
		NewTextInput("new passphrase again", &confirm).Masked().Required(),
	).WithSubmitFn(func(r ModalResponse) error {
		// COMPARED AFTER COMMIT, when both bindings hold this modal's values.
		// A row validator cannot do it: validators run before any binding is
		// written, so the other field's variable is still whatever it held
		// before the modal opened.
		if newPass != confirm {
			return ErrPassphraseMismatch
		}
		if oldPass == newPass {
			return errors.New("the new passphrase is the same as the current one")
		}
		if !m.authTask("change passphrase", func(c context.Context, b *Bound) error {
			return b.ChangePassphrase(c, oldPass, newPass)
		}) {
			return errors.New("another authentication attempt is still running — retry in a moment")
		}
		return nil
	}).Open()
	// NOT SCRIMMED. The requirement names login and quit as the only two
	// surfaces that fade the backdrop, and a passphrase change is not one of
	// them: it is a modal over a working application, and the operator may
	// well want to read what is behind it.
}
