package tui

import (
	"fmt"
	"strings"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/golib/tui"
)

// SWITCHING LANGUAGE — Options › Language.
//
// The language belongs to the App: the labels bind catalog messages
// (qsTrId in the QML, tui.Msg in the projected rows), and the widgets look
// each id up as they are laid out. Switching is one call; golib relabels the
// whole tree in one frame, and focus, typed text, scroll and open dialogs all
// stay.
//
// The one rule the mnemonic carries everywhere: the access key is English's
// letter, whatever the translation marked. Alt+F opens File in every
// language, by golib's run-time rule.

// languageName is what a row of the Language menu says: the language's own
// name for itself, a literal, never translated — the one row every locale
// reads the same way.
var languageNames = map[string]string{
	"en":    "English",
	"ko_KR": "한국어",
	"ja_JP": "日本語",
	"zh_CN": "中文",
	"es":    "Español",
	"pt_BR": "Português (Brasil)",
}

// languageState is what the menu reads: which language's row is checked.
func languageState(tag string) map[string]any {
	st := map[string]any{"App.language": tag}
	for _, l := range auth.Languages {
		st["App.lang_"+l] = l == tag
	}
	return st
}

// useLanguage switches the screen to tag. A language this build does not
// offer is refused, and the screen keeps its language.
func (h *Host) useLanguage(tag string) {
	if !auth.Contains(auth.Languages, tag) {
		h.setStatus(fmt.Sprintf("no language %q", tag))
		return
	}
	// Applied, not scheduled: the menu's checks move in this turn, and so
	// does the language, so text composed after the switch (setStatus) is
	// already in the new one.
	h.p.App().ApplyLanguage(tag)
	for k, v := range languageState(tag) {
		h.set(k, v)
	}
}

// languageOf is the language a stored preference resolves to: the tag when
// this build offers it, English when it does not. The stored option itself is
// never rewritten here — a build that offers the language must read it again.
func languageOf(pref string) string {
	if auth.Contains(auth.Languages, pref) {
		return pref
	}
	return "en"
}

// statusMessage is a catalog message for the status line: it follows
// a later language switch, unlike a line composed around a value.
func (h *Host) statusMessage(id string) {
	h.set("App.status", tui.Msg(id))
}

// statusMessageAround is a status line built around arg, in the current
// language: composed once, so it keeps the language it was built in.
func (h *Host) statusMessageAround(id, arg string) {
	h.setStatus(strings.ReplaceAll(h.p.App().Translate(tui.Msg(id)), "%1", arg))
}