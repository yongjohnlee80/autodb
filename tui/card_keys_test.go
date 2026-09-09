package tui

import (
	"strings"
	"testing"
)

// `y` MUST NOT BE CLAIMED BY THE CARD.
//
// It used to copy the token, which made a visual selection UNCOPYABLE: the
// card took the key before the read-only editor beneath could treat it as a
// yank. `y` now means what it means everywhere else — copy what I selected —
// and `Y` takes the whole thing. Asserted on the binding table, because that
// is where the mistake was.
func TestCard_CopyKeysAreYankFriendly(t *testing.T) {
	// The REAL tables, not a copy written here: the first version of this cell
	// asserted literals it had declared itself, which proves the test and not
	// the code.
	for _, tc := range []struct {
		what   string
		copies []cardCopy
	}{
		{"the token card", cardCopyKeys("the whole card", "body")},
		{"the CA certificate", cardCopyKeys("the CA certificate", "pem")},
	} {
		for _, cp := range tc.copies {
			if cp.key == 'y' {
				t.Errorf("%s claims lowercase y, so a visual selection cannot be yanked", tc.what)
			}
		}
		if len(tc.copies) != 1 || tc.copies[0].key != 'Y' {
			t.Errorf("%s does not bind Y to the whole content: %+v", tc.what, tc.copies)
		}
		if tc.copies[0].value == "" {
			t.Errorf("%s binds Y to nothing", tc.what)
		}
	}
}

// AND THE FOOTER SAYS WHAT THE KEYS ARE.
//
// They lived in the float's TITLE, which is read once — before there is
// anything to copy. The footer is also what feeds the `?` overlay, from the
// same list, so the two cannot disagree.
func TestCard_FooterNamesTheAvailableKeys(t *testing.T) {
	card := &connCard{keys: cardKeyHints("copy everything")}
	line := hintLine(card.hints())
	for _, want := range []string{"v/V then y", "copy a selection", "Y:copy everything", "q/Esc"} {
		if !strings.Contains(line, want) {
			t.Errorf("the footer does not offer %q: %s", want, line)
		}
	}
	// hints() IS the footer's source, so the overlay cannot drift from it.
	if len(card.hints()) != 3 {
		t.Errorf("hints() and the footer disagree: %v", card.hints())
	}
}

// THE CA SURFACE IS OFFERED TO EVERYONE.
//
// A CA certificate is public by construction — it is the file you hand out —
// and every developer configuring a client needs it. The two entries that ARE
// admin-only carry "(admin)" in their label; this one must not be gated with
// them.
func TestLeaderEntries_CACertificateIsOfferedToNonAdmins(t *testing.T) {
	for _, role := range []string{"admin", "editor", "reader"} {
		entries := leaderKeys(asRole(t, role))
		label, ok := entries['k']
		if !ok {
			t.Errorf("role %q is not offered SPC k (the CA certificate), which every client "+
				"configuration needs", role)
			continue
		}
		if strings.Contains(label, "(admin)") {
			t.Errorf("SPC k is labelled admin-only: %q", label)
		}
	}
	// And it did not collide with the keyslot, which is uppercase K.
	admin := leaderKeys(asRole(t, "admin"))
	if !strings.Contains(admin['K'], "keyslot") {
		t.Errorf("SPC K is no longer the keyslot: %q", admin['K'])
	}
}
