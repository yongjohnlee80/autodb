package tui

import "fmt"

// THE APP SINGLETON'S STATE — what the document reads.
//
// Each is a SOURCE, so changing one repaints exactly the bindings that read it.
// The host changes them only through the setters below, so what the screen
// says and what the host knows cannot drift apart.

// state is App's state with its starting values; theme is the one the layout
// imports.
func (h *Host) state(theme string) map[string]any {
	st := map[string]any{
		// The last message, on the status line's right.
		"App.status": "",
		// Where sign-in stands: "connecting", "disconnected", "bootstrap",
		// "login" or "signed-in". A document opens its dialogs on it.
		"App.auth": "connecting",
		// The status line's slots: the backend on the left, who is signed
		// in (and, with the workspace, where and which note) in the centre;
		// the last message is App.status, on the right.
		"App.statusLeft":   "Backend [disconnected]",
		"App.statusCenter": "",
		// The catalog as the document sees it (menu.go).
		"App.menu":       h.menus.bar,
		"App.leader":     h.menus.leader,
		"App.leaderText": "",
		"App.helpText":   "",
		// The overlays' text.
		"App.hints":        "",
		"App.aboutText":    h.aboutText(),
		"App.inspectRows":  h.inspectRows,
		"App.valueTitle":   "",
		"App.valueText":    "",
		"App.quitQuestion": "This ends the session. Anything unsaved in the query buffer is lost.",
		// Sign-in (auth.go): the name last tried, and why the last answer
		// was refused — each dialog's help line.
		"App.lastUser":       "",
		"App.loginError":     "",
		"App.bootstrapError": "",
	}
	// The workspace (workspace.go, explorer.go, results.go).
	st["App.queryTitle"] = h.queryTitle()
	st["App.explorer"] = h.explorer.model
	st["App.keyset"] = keysetValue(h.prefs.pref)
	for k, v := range resultsState(h.results) {
		st[k] = v
	}
	st["App.pressure"] = h.pressure.model
	st["App.pressureAge"] = ""
	st["App.cardTitle"] = ""
	st["App.cardText"] = ""
	for k, v := range noteState(h) {
		st[k] = v
	}
	for k, v := range zoomState() {
		st[k] = v
	}
	for k, v := range pickerState(h) {
		st[k] = v
	}
	for k, v := range connectionsState(h) {
		st[k] = v
	}
	for k, v := range tokenState(h) {
		st[k] = v
	}
	for k, v := range workspaceManagerState(h) {
		st[k] = v
	}
	for k, v := range confirmState() {
		st[k] = v
	}
	for k, v := range themeState(theme) {
		st[k] = v
	}
	return st
}

// setStatus puts a message on the status line.
func (h *Host) setStatus(msg string) { h.set("App.status", msg) }

// setAuth records where sign-in stands. What the catalog offers depends on
// it, so the menus are projected again.
func (h *Host) setAuth(state string) {
	h.auth = state
	h.set("App.auth", state)
	h.reproject()
}

// set writes one source; an error — a name no source declares — is the host's
// own mistake, and is kept for Run to return.
func (h *Host) set(name string, v any) {
	if h.p == nil {
		return
	}
	if err := h.p.Set(name, v); err != nil {
		h.keep(err)
	}
}

// keep records an error with nowhere else to go; Run returns it.
func (h *Host) keep(err error) {
	if err != nil {
		h.errs = append(h.errs, fmt.Errorf("tui: host: %w", err))
	}
}

// refreshIdentity brings the status line's backend and user up to date with
// the session.
func (h *Host) refreshIdentity() {
	h.set("App.statusLeft", backendText(h.session))
	h.set("App.statusCenter", h.where())
	h.set("App.aboutText", h.aboutText()) // the backend line follows the connection
	h.reproject()                         // who is signed in decides what is offered
}

// backendText is the backend as the status line says it.
func backendText(s *Session) string {
	pid, addr := s.ServerStatus()
	switch {
	case addr == "":
		return "Backend [disconnected]"
	case pid == 0:
		return "Backend " + addr // an older server does not report its pid
	default:
		return fmt.Sprintf("Backend [PID:%d] %s", pid, addr)
	}
}
