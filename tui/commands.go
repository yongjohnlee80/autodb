package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/yongjohnlee80/golib/decl"
	"github.com/yongjohnlee80/golib/parse/qml"
	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
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
		"App.run":                oneString("App.run", "a command id", h.run),
		"App.leaderKey":          oneString("App.leaderKey", "a command id", h.leaderKey),
		"App.quitConfirmed":      none(func() error { h.quitProgram(); return nil }),
		"App.showHints":          none(h.showHints),
		"App.login":              twoStrings("App.login", h.login),
		"App.bootstrap":          threeStrings("App.bootstrap", h.bootstrap),
		"App.signInDeclined":     none(h.signInDeclined),
		"App.explorerActivated":  oneIndex("App.explorerActivated", h.explorerActivated),
		"App.queryEdited":        none(h.queryEdited),
		"App.inspectResult":      oneNumber("App.inspectResult", "a result row", h.inspectResult),
		"App.openValue":          oneNumber("App.openValue", "a column row", h.openValue),
		"App.copyInspected":      oneNumber("App.copyInspected", "a column row", h.copyInspected),
		"App.copyValue":          none(h.copyValue),
		"App.pressureOpened":     none(h.pressureOpened),
		"App.pressureClosed":     none(h.pressureClosed),
		"App.copyCard":           none(h.copyCard),
		"App.cardClosed":         none(h.cardClosed),
		"App.tokenCreate":        none(h.tokenCreate),
		"App.tokenFormClosed":    none(h.tokenFormClosed),
		"App.tokensClosed":       none(h.tokensClosed),
		"App.tokenRevoke":        oneNumber("App.tokenRevoke", "a token row", h.tokenRevoke),
		"App.tokenToggleRevoked": none(h.tokenToggleRevoked),
		"App.mintToken": strings_("App.mintToken", 5, func(v []string) error {
			return h.mintToken(v[0], v[1], v[2], v[3], v[4])
		}),
		"App.workspaceChosen":          oneNumber("App.workspaceChosen", "a workspace row", h.workspaceChosen),
		"App.workspaceNew":             none(h.workspaceNew),
		"App.workspaceManagerClosed":   none(h.workspaceManagerClosed),
		"App.workspaceNameCancelled":   none(h.workspaceNameCancelled),
		"App.workspaceAttachCancelled": none(h.workspaceAttachCancelled),
		"App.changePassphrase":         threeStrings("App.changePassphrase", h.changePassphrase),
		"App.profileClosed":            none(h.profileClosed),
		"App.usersClosed":              none(h.usersClosed),
		"App.userFormCancelled":        none(h.userFormCancelled),
		"App.userAdd":                  none(h.userAdd),
		"App.userRole":                 oneNumber("App.userRole", "a user row", h.userRole),
		"App.userResetPassphrase":      oneNumber("App.userResetPassphrase", "a user row", h.userResetPassphrase),
		"App.userToggle":               oneNumber("App.userToggle", "a user row", h.userToggle),
		"App.userGrant":                oneNumber("App.userGrant", "a user row", h.userGrant),
		"App.userIPs":                  oneNumber("App.userIPs", "a user row", h.userIPs),
		"App.userRemove":               oneNumber("App.userRemove", "a user row", h.userRemove),
		"App.saveUser": strings_("App.saveUser", 4, func(v []string) error {
			return h.saveUser(v[0], v[1], v[2], v[3])
		}),
		"App.addressesClosed":   none(h.addressesClosed),
		"App.addressAdd":        none(h.addressAdd),
		"App.addressRemove":     oneNumber("App.addressRemove", "an IP row", h.addressRemove),
		"App.addressSave":       twoStrings("App.addressSave", h.addressSave),
		"App.workspaceRename":   oneNumber("App.workspaceRename", "a workspace row", h.workspaceRename),
		"App.workspaceDelete":   oneNumber("App.workspaceDelete", "a workspace row", h.workspaceDelete),
		"App.workspaceAttach":   oneNumber("App.workspaceAttach", "a workspace row", h.workspaceAttachTo),
		"App.workspaceDetach":   twoNumbers("App.workspaceDetach", h.workspaceDetach),
		"App.saveWorkspace":     oneString("App.saveWorkspace", "a workspace name", h.saveWorkspace),
		"App.attachToWorkspace": workspaceAndName(h.attachToWorkspace),
		"App.movePane":          oneString("App.movePane", "h, j, k or l", h.movePane),
		"App.chooseConnection":  oneNumber("App.chooseConnection", "a row's index", h.chooseConnection),
		"App.confirmed":         oneString("App.confirmed", "yes or no", h.confirmed),
		"App.connectionAdd":     none(h.connectionAdd),
		"App.connectionEdit":    oneNumber("App.connectionEdit", "a row's index", h.connectionEdit),
		"App.connectionTest":    oneNumber("App.connectionTest", "a row's index", h.connectionTest),
		"App.connectionDelete":  oneNumber("App.connectionDelete", "a row's index", h.connectionDelete),
		"App.connectionAttach":  oneNumber("App.connectionAttach", "a row's index", h.connectionAttach),
		"App.saveConnection": strings_("App.saveConnection", 5, func(v []string) error {
			return h.saveConnection(v[0], v[1], v[2], v[3], v[4])
		}),
		"App.attachConnection": workspaceAndName(h.attachConnection),
		"App.nameNote":         workspaceAndName(h.nameNote),
		"App.unsaved":          oneString("App.unsaved", "save, discard or stay", h.unsavedAnswered),
		"App.conflict":         oneString("App.conflict", "overwrite, saveas or keep", h.conflictAnswered),
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

// strings is a command that takes n strings, in order.
func strings_(name string, n int, fn func([]string) error) decl.HandlerFunc {
	return func(args []qml.SpecValue) error {
		if len(args) != n {
			return fmt.Errorf("%s takes %d strings, and was given %d arguments", name, n, len(args))
		}
		vals := make([]string, n)
		for i, a := range args {
			if a.Kind != qml.SpecValueString {
				return fmt.Errorf("%s takes %d strings; argument %d is not one", name, n, i+1)
			}
			vals[i] = a.Raw
		}
		return fn(vals)
	}
}

// oneNumber is a command that takes one whole number — a list row's index.
func oneNumber(name, what string, fn func(int) error) decl.HandlerFunc {
	return func(args []qml.SpecValue) error {
		if len(args) != 1 || args[0].Kind != qml.SpecValueNumber {
			return fmt.Errorf("%s takes %s", name, what)
		}
		n, err := strconv.Atoi(args[0].Raw)
		if err != nil {
			return fmt.Errorf("%s takes %s, not %s", name, what, args[0].Raw)
		}
		return fn(n)
	}
}

func twoNumbers(name string, fn func(int, int) error) decl.HandlerFunc {
	return func(args []qml.SpecValue) error {
		if len(args) != 2 || args[0].Kind != qml.SpecValueNumber || args[1].Kind != qml.SpecValueNumber {
			return fmt.Errorf("%s takes two row indexes", name)
		}
		a, err := strconv.Atoi(args[0].Raw)
		if err != nil {
			return fmt.Errorf("%s: first row: %w", name, err)
		}
		b, err := strconv.Atoi(args[1].Raw)
		if err != nil {
			return fmt.Errorf("%s: second row: %w", name, err)
		}
		return fn(a, b)
	}
}

// oneIndex is a command that takes a view's row Index, as a signal gives it.
func oneIndex(name string, fn func(tuidecl.Index) error) decl.HandlerFunc {
	return func(args []qml.SpecValue) error {
		if len(args) != 1 {
			return fmt.Errorf("%s takes a row's index", name)
		}
		ix, ok := args[0].Obj.(tuidecl.Index)
		if !ok {
			return fmt.Errorf("%s takes a row's index, not %s", name, args[0].Raw)
		}
		return fn(ix)
	}
}

// workspaceAndName is App.nameNote(workspace, name): a workspace id, as a
// ComboBox's currentValue gives it, and a name.
func workspaceAndName(fn func(int64, string) error) decl.HandlerFunc {
	return func(args []qml.SpecValue) error {
		if len(args) != 2 || args[1].Kind != qml.SpecValueString {
			return fmt.Errorf("App.nameNote takes a workspace and a name")
		}
		if args[0].Raw == "" {
			return fn(0, args[1].Raw) // nothing chosen: the host says so
		}
		ws, err := strconv.ParseInt(args[0].Raw, 10, 64)
		if err != nil {
			return fmt.Errorf("App.nameNote: %q is not a workspace id", args[0].Raw)
		}
		return fn(ws, args[1].Raw)
	}
}

// twoStrings is a command that takes two strings.
func twoStrings(name string, fn func(a, b string) error) decl.HandlerFunc {
	return strings_(name, 2, func(v []string) error { return fn(v[0], v[1]) })
}

// threeStrings is a command that takes three strings.
func threeStrings(name string, fn func(a, b, c string) error) decl.HandlerFunc {
	return strings_(name, 3, func(v []string) error { return fn(v[0], v[1], v[2]) })
}

// open opens a dialog the document declares, by its id — the one thing a
// command does that the document cannot decide itself: which dialog a
// command's moment calls for.
func (h *Host) open(id string) {
	if err := h.p.Call(id, "open"); err != nil {
		h.keep(err)
	}
}
