package tui

import (
	"fmt"
	"strings"

	"github.com/yongjohnlee80/golib/decl"
	"github.com/yongjohnlee80/golib/parse/qml"
)

// THE APP SINGLETON'S COMMANDS — what the document invokes.
//
// The table is the whole of it: a handler in main.qml or a component file can
// reach exactly these, by these names, and nothing else of the program.
//
// A CATALOG COMMAND runs through App.run(id), and only through it: the command
// is re-resolved — offered, for this user, in this state — immediately before
// it runs, so there is no second handler map to drift from the catalog and no
// way for a document to run what the catalog would not offer. The other
// handlers carry what a dialog was answered, or what a view reports, into the
// host (editor-qml's App.openFile(path) is one); none runs a catalog command
// except through the catalog.
func (h *Host) commands() map[string]decl.HandlerFunc {
	return map[string]decl.HandlerFunc{
		"App.run":           oneString("App.run", "a command id", h.run),
		"App.leaderKey":     oneString("App.leaderKey", "a command id", h.leaderKey),
		"App.quitConfirmed": none(func() error { h.quitProgram(); return nil }),
		"App.showHints":     none(h.showHints),
	}
}

// run is App.run: a catalog command, through its one activation path.
func (h *Host) run(id string) error {
	cmd, known := h.catalog.Command(CommandID(id))
	if !known {
		return fmt.Errorf("App.run: %q is not a command", id)
	}
	if !h.catalog.runIfOffered(h, cmd.ID) {
		h.setStatus(notOffered(cmd.offering(h), id))
	}
	return nil
}

// leaderKey is a key pressed in the leader menu. An offered command closes the
// menu and runs; a disabled one leaves the menu open and says why — closing it
// on a dead key would read as though the command had run.
func (h *Host) leaderKey(id string) error {
	cmd, known := h.catalog.Command(CommandID(id))
	if !known {
		return fmt.Errorf("App.leaderKey: %q is not a command", id)
	}
	if off := cmd.offering(h); off.State != OfferOffered {
		h.setStatus(notOffered(off, id))
		return nil
	}
	// Closed FIRST: a command that opens another surface must not find the
	// leader still on top.
	if err := h.p.Call("leader", "close"); err != nil {
		return err
	}
	h.catalog.runIfOffered(h, cmd.ID)
	return nil
}

// notOffered is what the status line says about a command that did not run.
func notOffered(off Offering, id string) string {
	if off.State == OfferDisabled {
		return off.Reason
	}
	return "not available now: " + id
}

// showHints is `?`: the keys that work where the user is, on a card.
func (h *Host) showHints() error {
	h.set("App.hints", h.hints())
	return h.p.Call("hints", "open")
}

// hints are the keys that work at the top level of the screen.
func (h *Host) hints() string {
	return strings.Join([]string{
		"SPC       the leader menu: every command, by key",
		"F10       the menu bar; Alt+letter opens a menu",
		"q         quit (asks first)",
		"Ctrl+Q    quit (asks first)",
		"?         these keys",
	}, "\n")
}

// none is a command that takes no arguments, and refuses any it is given.
func none(fn func() error) decl.HandlerFunc {
	return func(args []qml.SpecValue) error {
		if len(args) > 0 {
			return fmt.Errorf("takes no arguments, and was given %d", len(args))
		}
		return fn()
	}
}

// oneString is a command that takes one string.
func oneString(name, what string, fn func(string) error) decl.HandlerFunc {
	return func(args []qml.SpecValue) error {
		if len(args) != 1 || args[0].Kind != qml.SpecValueString {
			return fmt.Errorf("%s takes %s", name, what)
		}
		return fn(args[0].Raw)
	}
}

// open opens a dialog the document declares, by its id — the one thing a
// command does that the document cannot decide itself: which dialog a
// command's moment calls for.
func (h *Host) open(id string) {
	if err := h.p.Call(id, "open"); err != nil {
		h.keep(err)
	}
}
