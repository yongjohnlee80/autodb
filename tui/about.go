package tui

import (
	"strconv"
	"strings"
)

// ABOUT — what this build is, who wrote it, and WHERE its state lives. The
// last part is the operational half: "which database am I actually using" was
// a real question during M6 testing, and the answer should not require reading
// source or `lsof`.
//
// Everything here is CLIENT-side knowledge: the binary's own build stamps, the
// local config's resolved paths, and the backend identity the handshake
// already reports. Nothing is fetched from the server, so it works before
// anyone has signed in.

// AboutInfo is the build and location detail the program displays, supplied
// by the runner (Options.About).
type AboutInfo struct {
	Version    string
	Commit     string
	BuildDate  string
	Repo       string
	Author     string
	NotesDir   string
	MetaEngine string
	MetaPath   string // sqlite file, or the postgres DSN's host for pg
	ConfigPath string // "" when no config file is in play (defaults)
}

// canRestartDaemon reports whether daemon-restart actions can COMPLETE here.
//
// Two conditions, and the second was once missing. A terminal frontend is
// necessary — nothing in the web process can start a daemon — but it is not
// sufficient: what a restart needs is a spawner, because the disconnect
// watcher starts the replacement from Session.spawn. On a service install the
// client config carries client_only = true, so the spawner is nil by design;
// an operator on the droplet pressed SPC X and the front door stayed down,
// because systemd restarts on FAILURE and a clean shutdown is not one.
func (h *Host) canRestartDaemon() bool {
	return h.frontend == FrontendTerminal && h.session != nil && h.session.CanSpawn()
}

// aboutRows are About's label/value lines.
func (h *Host) aboutRows() [][2]string {
	info := h.about
	backend := "not connected"
	if h.session != nil {
		pid, addr := h.session.ServerStatus()
		switch {
		case addr == "":
			backend = "not connected (" + h.session.addr + " configured)"
		case pid > 0:
			backend = "[PID:" + strconv.FormatInt(pid, 10) + "] " + addr
		default:
			backend = addr
		}
	}
	meta := info.MetaPath
	if info.MetaEngine != "" {
		meta = info.MetaEngine + " · " + meta
	}
	cfg := info.ConfigPath
	if cfg == "" {
		cfg = "(none — built-in defaults)"
	}
	rows := [][2]string{
		{"version", firstNonEmpty(info.Version, "dev")},
		{"commit", firstNonEmpty(info.Commit, "none")},
		{"built", firstNonEmpty(info.BuildDate, "unknown")},
		{"author", info.Author},
		{"repository", info.Repo},
		{"", ""},
		{"backend", backend},
	}
	if h.session != nil {
		if bv := h.session.ServerVersion(); bv != "" {
			rows = append(rows, [2]string{"backend build", backendBuildLine(info.Version, bv, h.canRestartDaemon())})
		}
	}
	return append(rows,
		[2]string{"meta store", meta},
		[2]string{"notes", h.notesLine()},
		[2]string{"config", cfg},
	)
}

// aboutText is About's body: the rows, their labels aligned.
func (h *Host) aboutText() string {
	rows := h.aboutRows()
	width := 0
	for _, r := range rows {
		width = max(width, len(r[0]))
	}
	var b strings.Builder
	for _, r := range rows {
		if r[0] == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(r[0] + strings.Repeat(" ", width-len(r[0])) + "  " + r[1] + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// notesLine reports the root this session ACTUALLY reads, which is not the
// configured base. Before sign-in there is no identity and therefore no
// personal root, so About says so rather than naming the base: that would name
// a directory this session never reads (the identity-keyed notes design).
func (h *Host) notesLine() string {
	if h.notes != nil {
		return h.notes.Root()
	}
	if h.about.NotesDir == "" {
		return "(none configured)"
	}
	return "(resolved after sign-in — notes are per user)"
}

// backendBuildLine compares the RUNNING daemon's build against this binary's,
// and says plainly when they differ.
//
// This is the M6 footgun made visible. Twice during manual testing a feature
// "did nothing" because a daemon from an earlier build was still serving — the
// running process kept answering while its executable had been replaced
// underneath it. A shared daemon outlives the frontend that started it BY
// DESIGN, so this is what happens every time you rebuild and forget. The
// frontend cannot fix it (restart is the admin's call), so it names the
// mismatch and the remedy — the SPC X remedy only where the frontend has it.
func backendBuildLine(frontend, backend string, canRestart bool) string {
	if frontend == "" {
		frontend = "dev"
	}
	if frontend == backend {
		return backend + " (matches this binary)"
	}
	if canRestart {
		return backend + " ≠ " + frontend + " — the running server is a DIFFERENT build. " +
			"Restart it (SPC X) to pick up this one."
	}
	return backend + " ≠ " + frontend + " — the running server is a DIFFERENT build. " +
		"An administrator must restart it from a terminal to pick up this one."
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
