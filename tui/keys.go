package tui

import (
	"fmt"
	"strings"

	tuikey "github.com/yongjohnlee80/golib/tui/key"
	"github.com/yongjohnlee80/golib/tui/keymap"
)

// KEYS — every key AutoDB binds is a golib key command (golib tui/keymap): an id, a scope and its
// default keys, so a user's keymap.xml beside the config file can move it by id. The Window's are
// keymap.Global's (autodb.window.*); SPC's card holds one per catalog command with a leader key
// (autodb.leader.<command id>), live in the card's scope; History, Audit and Inspect each have a scope
// of their own. The QML names each by keyId.

// leaderScope is SPC's card's keys: views/Leader.qml's keyScope.
const leaderScope keymap.Scope = "autodb.leader"

// keyCmd declares a command: its scope is its id less the last part, autodb.window's the Window's.
func keyCmd(id string, keys ...string) keymap.Command {
	scope := keymap.Scope(id[:strings.LastIndex(id, ".")])
	if scope == "autodb.window" || strings.HasPrefix(string(scope), "autodb.window.") {
		scope = keymap.Global
	}
	return keymap.Command{ID: id, Scope: scope, Keys: tuikey.MustParse(keys...)}
}

// fixedCommands are the keys main.qml and the dialogs name by id.
var fixedCommands = []keymap.Command{
	keyCmd("autodb.window.leader", "Space"),
	keyCmd("autodb.window.quit", "Q", "Ctrl+Q"),
	keyCmd("autodb.window.hints", "?"),
	keyCmd("autodb.window.search", "/"),
	keyCmd("autodb.window.searchNext", "N"),
	keyCmd("autodb.window.searchPrevious", "Shift+N"),
	keyCmd("autodb.window.movePane.h", "Ctrl+H"),
	keyCmd("autodb.window.movePane.j", "Ctrl+J"),
	keyCmd("autodb.window.movePane.k", "Ctrl+K"),
	keyCmd("autodb.window.movePane.l", "Ctrl+L"),
	keyCmd("autodb.history.load", "E"),
	keyCmd("autodb.history.copy", "Y"),
	keyCmd("autodb.history.filter", "F"),
	keyCmd("autodb.history.clearFilter", "X"),
	keyCmd("autodb.history.nextPage", "N"),
	keyCmd("autodb.history.previousPage", "P"),
	keyCmd("autodb.audit.filter", "F"),
	keyCmd("autodb.audit.clearFilter", "X"),
	keyCmd("autodb.audit.nextPage", "N"),
	keyCmd("autodb.audit.previousPage", "P"),
	keyCmd("autodb.inspect.copy", "Y"),
}

// leaderCommand is a catalog command's leader key as a key command: autodb.leader.<id>.
func leaderCommand(id CommandID) string { return "autodb.leader." + string(id) }

// keyCommands are every key command AutoDB declares: the fixed ones, then one per catalog command
// with a leader key, its default the catalog's.
func keyCommands(cmds []CommandOf[*Host]) []keymap.Command {
	out := append([]keymap.Command(nil), fixedCommands...)
	for _, c := range cmds {
		if c.Leader == nil {
			continue
		}
		out = append(out, keymap.Command{ID: leaderCommand(c.ID), Scope: leaderScope, Keys: tuikey.MustParse(leaderSequence(c.Leader.Key))})
	}
	return out
}

// leaderKeyName is how SPC's card names a command's key: the catalog's letter while the user's scheme
// leaves it, else the keys the scheme gave it ("unbound" for none).
func (h *Host) leaderKeyName(id CommandID, def rune) string {
	if h.p == nil {
		return string(def) // no program yet: the catalog's letter
	}
	m := h.p.App().Keymap()
	cmdID := leaderCommand(id)
	c, ok := m.Command(cmdID)
	if !ok || m.SourceOf(cmdID).Kind == keymap.FromDefault {
		return string(def)
	}
	keys := m.KeysOf(c)
	if len(keys) == 0 {
		return "unbound"
	}
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = k.String()
	}
	return strings.Join(names, " ")
}

// keysChanged is the App's key map installed or replaced: SPC's card names the keys as it binds them.
func (h *Host) keysChanged() { h.projectLeader() }

// keyLine is a leader row's text: its key as the card names it, and its label.
func (h *Host) keyLine(id CommandID, def rune, label string) string {
	return fmt.Sprintf("%s  %s", h.leaderKeyName(id, def), label)
}
