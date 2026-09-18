package tui

import (
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/meta"
	tuicore "github.com/yongjohnlee80/golib/tui"
)

// PROFILE IS OFFERED TO AN EDITOR, unlike the Users manager beside it on the
// same menu. The one thing it does is change the CALLER'S OWN passphrase,
// which the daemon scopes to whoever the token resolves to; hiding it from
// editors would hide the only route they have to their own passphrase, which
// is the state this follow-up exists to end.
func TestProfile_IsOfferedToEveryRole(t *testing.T) {
	for _, role := range []string{meta.RoleAdmin, meta.RoleEditor} {
		t.Run(role, func(t *testing.T) {
			h := startBar(t, role)
			var found bool
			h.on(func() {
				for _, c := range h.m.catalog.commands {
					if c.ID != cmdProfile {
						continue
					}
					found = c.Visible == nil || c.Visible(h.m)
				}
			})
			if !found {
				t.Fatalf("Profile is not offered to a %s", role)
			}
		})
	}
}

// AND IT IS HIDDEN WHILE NOBODY IS SIGNED IN. The cell above passes for a
// command that is always visible, including on a screen where there is no
// account for it to be about.
func TestProfile_IsHiddenBeforeSignIn(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	var visible bool
	h.on(func() {
		h.m.session.user = UserInfo{}
		for _, c := range h.m.catalog.commands {
			if c.ID == cmdProfile {
				visible = c.Visible == nil || c.Visible(h.m)
			}
		}
	})
	if visible {
		t.Fatal("Profile is offered while nobody is signed in")
	}
}

// THE PROFILE CARD NAMES THE ACCOUNT AND WHAT THE CHANGE COSTS. An operator
// deciding whether to change a passphrase needs to know the current one is
// required, because an account whose owner has forgotten it needs an
// administrator and a different operation.
func TestProfile_NamesTheAccountAndTheRequirement(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	h.on(func() { h.m.openProfile() })
	h.waitUntil("the profile card is open", func() bool { return h.m.modalOpen() })
	h.settle()

	got := h.screen()
	for _, want := range []string{"op", meta.RoleAdmin, "current one", "Change passphrase"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is not on the profile card:\n%s", want, got)
		}
	}
}

// A MISTYPED CONFIRMATION IS REFUSED, AND THE MODAL STAYS OPEN.
//
// The field is masked, so a typo is invisible; and the failure it produces is
// not "the new passphrase is wrong" but "you can no longer sign in",
// discovered at the next login with nothing left to compare against. The
// daemon cannot catch it — it is told one new passphrase and has nothing to
// check it against.
func TestProfile_AMistypedConfirmationIsRefused(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	h.on(func() { h.m.openChangePassphrase() })
	h.waitUntil("the change form is open", func() bool { return h.m.modalOpen() })

	typeText(h, "current-one")
	h.key(tuicore.KeyEnter)
	typeText(h, "the-new-one")
	h.key(tuicore.KeyEnter)
	typeText(h, "the-new-onf") // one key off, and invisible behind the mask
	h.key(tuicore.KeyEnter)    // to OK
	h.key(tuicore.KeyEnter)    // activate
	h.settle()

	var open bool
	h.on(func() { open = h.m.modalOpen() })
	if !open {
		t.Fatal("a mistyped confirmation closed the form; the operator would learn at the next login")
	}
	if !strings.Contains(h.screen(), ErrPassphraseMismatch.Error()) {
		t.Fatalf("the form does not say the two entries differ:\n%s", h.screen())
	}
}

// AND REUSING THE CURRENT PASSPHRASE IS REFUSED TOO. The cell above also
// passes for a form that refuses every submit.
func TestProfile_TheNewPassphraseMustDiffer(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	h.on(func() { h.m.openChangePassphrase() })
	h.waitUntil("the change form is open", func() bool { return h.m.modalOpen() })

	for i, s := range []string{"same-one", "same-one", "same-one"} {
		typeText(h, s)
		h.key(tuicore.KeyEnter)
		_ = i
	}
	h.key(tuicore.KeyEnter)
	h.settle()

	if !strings.Contains(h.screen(), "same as the current one") {
		t.Fatalf("reusing the current passphrase was not refused:\n%s", h.screen())
	}
}

// THE PASSPHRASE IS NOT TRIMMED. A masked field's leading and trailing spaces
// are the operator's, and silently discarding them makes a passphrase that
// cannot be typed back.
func TestProfile_APassphraseKeepsItsSpaces(t *testing.T) {
	h := startBar(t, meta.RoleAdmin)
	var got string
	h.on(func() {
		NewInputModal(h.m, "masked", NewTextInput("secret", &got).Masked().Required()).
			WithSubmitFn(func(ModalResponse) error { return nil }).Open()
	})
	h.waitUntil("the form is open", func() bool { return h.m.modalOpen() })
	typeText(h, " padded ")
	h.key(tuicore.KeyEnter)
	h.key(tuicore.KeyEnter)
	h.waitUntil("the form closed", func() bool { return !h.m.modalOpen() })

	if got != " padded " {
		t.Fatalf("a masked field committed %q; the spaces are the operator's", got)
	}
}
