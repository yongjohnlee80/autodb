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
		// The status line: the backend, who is signed in, the last message.
		"App.backend":  "Backend [disconnected]",
		"App.identity": "",
		"App.status":   "",
		// Where sign-in stands: "connecting", "disconnected", "bootstrap",
		// "login" or "signed-in". A document opens its dialogs on it.
		"App.auth": "connecting",
	}
	for k, v := range themeState(theme) {
		st[k] = v
	}
	return st
}

// setStatus puts a message on the status line.
func (h *Host) setStatus(msg string) { h.set("App.status", msg) }

// setAuth records where sign-in stands.
func (h *Host) setAuth(state string) { h.set("App.auth", state) }

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
	h.set("App.backend", backendText(h.session))
	h.set("App.identity", h.session.User().Name)
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
