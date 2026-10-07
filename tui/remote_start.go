package tui

import (
	tuidecl "github.com/yongjohnlee80/golib/tui/decl"

	"github.com/yongjohnlee80/autodb/core/remoteclient"
)

// WHERE THE TERMINAL STARTS — [tui] start and --remote.
//
// A start on a remote server opens Connect at that profile and dials
// nothing else: the local daemon is neither dialed nor started unless the
// user asks for it (SPC x, or choosing this computer). Cancelling that first
// Connect leaves the program disconnected, saying how to go on, rather than
// quietly falling back to a local daemon it might have to start.

// Start is where the terminal's session begins.
type Start struct {
	// Remote is the id of a remotes.toml profile to connect to instead of
	// the local daemon; "" for none.
	Remote string
	// Ask asks at start: this computer, or one of the remote servers.
	Ask bool
}

// notStartedHint is the status of a program that started connected to
// nothing.
const notStartedHint = "not connected — Remote › Connect… reaches a remote server; SPC x this computer's daemon"

func startState(h *Host) map[string]any {
	return map[string]any{"App.startChoices": h.startChoices}
}

// startAt begins the terminal's session where Options.Start says.
func (h *Host) startAt() {
	switch {
	case h.startOpt.Remote != "":
		h.startRemote(h.startOpt.Remote)
	case h.startOpt.Ask:
		h.askStart()
	default:
		h.connect()
	}
}

// startRemote opens Connect at profile id, nothing dialed yet.
func (h *Host) startRemote(id string) {
	h.awaitingStart = true
	h.setAuth("disconnected")
	h.statusMessageAround("autodb.remote_start.status.around", id)
	h.refreshIdentity()
	h.openRemoteConnectAt(id)
}

// askStart asks where to start: this computer first, then each remote
// server. With no remote server there is nothing to ask.
func (h *Host) askStart() {
	path, err := h.profilesPath()
	var profiles []remoteclient.Profile
	if err == nil {
		profiles, err = remoteclient.LoadProfiles(path)
	}
	if err != nil {
		h.statusMessageAround("autodb.remote_start.status.around2", err.Error())
		h.connect()
		return
	}
	if len(profiles) == 0 {
		h.connect()
		return
	}
	h.startProfiles = profiles
	rows := []tuidecl.Row{{"key": "", "label": "This computer — the local autodb daemon"}}
	for _, p := range profiles {
		rows = append(rows, tuidecl.Row{"key": p.ID,
			"label": profileLabel(p) + " — " + p.Address() + " as " + p.User})
	}
	h.startChoices.Reset(rows)
	h.awaitingStart = true
	h.setAuth("disconnected")
	h.refreshIdentity()
	h.open("startChoice")
}

// startChosen is App.startChosen(i): 0 this computer, else a remote server.
func (h *Host) startChosen(i int) error {
	if !h.awaitingStart {
		return nil
	}
	if err := h.p.Call("startChoice", "close"); err != nil {
		h.keep(err)
	}
	switch {
	case i == 0:
		h.awaitingStart = false
		h.connect()
	case i > 0 && i <= len(h.startProfiles):
		h.startRemote(h.startProfiles[i-1].ID)
	}
	return nil
}

// startDeclined is App.startDeclined: the start question closed unanswered.
func (h *Host) startDeclined() error {
	if h.awaitingStart && !h.session.Connected() {
		h.awaitingStart = false
		h.setStatus(notStartedHint)
	}
	return nil
}
