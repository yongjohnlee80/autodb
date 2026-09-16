package tui

// THE FIRST Planned LEAF TO RETIRE.
//
// Declaring a lifecycle is only worth something if a leaf can leave it, and
// these two are the first to do so: hidden while the preference had nowhere to
// live, offered now that v17 gave it one. The cells below are about the leaves
// as the operator meets them — the label, the gate, and the mapping onto the
// profile golib actually builds.

import (
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/tui/widget"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// THE LABEL SAYS TEXTEDIT, AND SO DOES THE ID.
//
// The Phase 1 menu tree said "Nano mode" and the acceptance requirement says
// TextEdit; the requirement wins. Asserting BOTH halves is the point: a label
// corrected while the id still said nano would read correctly and sort, search
// and log as something else.
func TestEditorLeaves_NameTextEditInBothTheIDAndTheLabel(t *testing.T) {
	m := leaderModelFor(t, leaderState{role: meta.RoleAdmin, frontend: FrontendTerminal})
	rows := m.menuModel()

	vim, ok := find(rows, widget.ItemID("options.editor.vim"))
	if !ok {
		t.Fatal("the Vim leaf is missing")
	}
	if vim.Label != "Vim mode" {
		t.Errorf("vim label = %q, want %q", vim.Label, "Vim mode")
	}

	if _, ok := find(rows, widget.ItemID("options.editor.nano")); ok {
		t.Error("options.editor.nano still exists; the id was corrected with the label")
	}
	te, ok := find(rows, widget.ItemID("options.editor.textedit"))
	if !ok {
		t.Fatal("the TextEdit leaf is missing")
	}
	if te.Label != "TextEdit mode" {
		t.Errorf("textedit label = %q, want %q", te.Label, "TextEdit mode")
	}
}

// RETIRED FROM Planned MEANS ENABLED, not merely present. A Planned leaf is
// shown disabled, so "the row is there" would pass without the leaf having
// retired at all.
func TestEditorLeaves_AreOfferedWhenSignedIn(t *testing.T) {
	m := leaderModelFor(t, leaderState{role: meta.RoleAdmin, frontend: FrontendTerminal})
	rows := m.menuModel()

	for _, id := range []string{"options.editor.vim", "options.editor.textedit"} {
		row, ok := find(rows, widget.ItemID(id))
		if !ok {
			t.Fatalf("%s is missing", id)
		}
		if !row.Enabled {
			t.Errorf("%s is present but disabled; it should have retired from Planned", id)
		}
	}
}

// SIGNED OUT, THE CHOICE IS SHOWN AND REFUSED, WITH THE REASON.
//
// A preference belongs to an account, so before a sign-in there is nowhere to
// write it. Dimmed rather than hidden: the operator is one login away from it,
// and a row that vanishes teaches that the feature does not exist.
func TestEditorLeaves_DimmedWithAReasonBeforeSignIn(t *testing.T) {
	m := leaderModelFor(t, leaderState{role: meta.RoleAdmin, frontend: FrontendTerminal})
	m.session.user = UserInfo{} // no account yet
	rows := m.menuModel()

	row, ok := find(rows, widget.ItemID("options.editor.textedit"))
	if !ok {
		t.Fatal("the TextEdit leaf vanished when signed out; it should dim, not hide")
	}
	if row.Enabled {
		t.Error("the leaf is offered with no account to store the preference on")
	}
	// THE REASON RIDES IN Accel, not in the label. The bar puts it in the
	// right-hand column where an accelerator would go; the leader menu appends
	// it to the label instead. Asserting the wrong one of those passes or fails
	// on the surface rather than on whether the operator is told anything.
	if !strings.Contains(strings.ToLower(row.Accel), "sign in") {
		t.Errorf("the disabled row says %q in its accel column, which does not tell "+
			"the operator why it is unavailable", row.Accel)
	}
}

// The product's two names meet golib's three profiles in exactly one place.
// TextEdit is golib's KeysetStandard, and anything unrecognised is Vim, because
// the editor must not be left without a profile by a value nobody planned for.
func TestKeysetOf_MapsTheProductNamesOntoGolibProfiles(t *testing.T) {
	for _, tc := range []struct {
		pref string
		want widget.Keyset
	}{
		{auth.KeysetVim, widget.KeysetVim},
		{auth.KeysetTextEdit, widget.KeysetStandard},
		{"", widget.KeysetVim},
		{"nano", widget.KeysetVim},
	} {
		if got := keysetOf(tc.pref); got != tc.want {
			t.Errorf("keysetOf(%q) = %v, want %v", tc.pref, got, tc.want)
		}
	}
}
