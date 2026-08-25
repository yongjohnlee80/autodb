package tui

import (
	"iter"
	"strconv"

	"github.com/yongjohnlee80/golib/tui"
	"github.com/yongjohnlee80/golib/tui/style"
	"github.com/yongjohnlee80/golib/tui/widget"
)

// About (SPC A, and once at startup): what this build is, who wrote it,
// and WHERE its state lives. The last part is the operational half —
// "which database am I actually using" was a real question during M6
// testing, and the answer should not require reading source or `lsof`.
//
// Everything here is CLIENT-side knowledge: the binary's own build
// stamps, the local config's resolved paths, and the backend identity
// the handshake already reports. Nothing is fetched from the server, so
// the splash works before anyone has logged in.

// AboutInfo is the build and location detail the frontend displays.
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

// WithAbout supplies the build/location detail for the About modal.
func WithAbout(info AboutInfo) Option { return func(m *Model) { m.about = info } }

// Option configures the Model at construction.
type Option func(*Model)

// Frontend says what this frontend can do about the daemon's lifetime.
//
// It exists because the shared component tree offers an admin action that SHUTS
// THE DAEMON DOWN, and whether that is safe depends entirely on who is hosting the
// tree. In a terminal it is: the daemon exits, the next call fails, and the
// session's spawn function starts a replacement. Under `autodb --web-ui` the spawn
// is nil by design (ADR-0061 §2.2), so the same keystroke strands every web session
// with no way to bring the daemon back — including sessions belonging to other
// people.
//
// The zero value is the terminal, so every existing caller keeps its behaviour
// without naming it.
type Frontend uint8

const (
	// FrontendTerminal may shut the daemon down; a spawn restores it.
	FrontendTerminal Frontend = iota
	// FrontendWeb must not: nothing in this process will start one.
	FrontendWeb
)

// WithFrontend declares the hosting frontend (ADR-0061 §2.7).
func WithFrontend(f Frontend) Option { return func(m *Model) { m.frontend = f } }

// NoteView describes WHICH note tree this session actually reads (ADR-0064 §2.3).
//
// The Model needs to be told, rather than inferring it from the frontend. A
// browser session reads its own root by DEFAULT and the shared workspace tree when
// the gateway is bound — and help text predicated on "is this the web frontend"
// said "you are reading your own root, set notes_mode=workspace to share" even to
// a session already in workspace mode, which was simply false (lector r1 on
// PR #5).
type NoteView struct {
	// Shared is true when this session reads the same workspace-keyed tree the
	// terminal TUI writes, false when it reads a private per-identity root.
	Shared bool
	// (No path here on purpose. About carries the effective root, set by
	// aboutForRoot; duplicating it in the view would be a second source of truth
	// for the same fact — lector r2 non-blocking note.)
}

// WithNoteView tells the Model which tree it is reading, so help explains the
// mode actually in force.
func WithNoteView(v NoteView) Option { return func(m *Model) { m.noteView = v } }

// NoteViewOf reports the note view a host configured. Exported so a host can
// assert its OWN wiring: the interesting bug is not "does the option work" but
// "did the runner pass it", and that is only observable on the built Model
// (lector r2 on PR #5).
func (m *Model) NoteViewOf() NoteView { return m.noteView }

// AboutNotesDir reports the note root About will display, for the same reason.
func (m *Model) AboutNotesDir() string { return m.notesLine(m.about) }

// canRestartDaemon reports whether this frontend may offer daemon-restart
// actions.
func (m *Model) canRestartDaemon() bool { return m.frontend == FrontendTerminal }

// managesOwnAuth reports whether this frontend authenticates its own session.
//
// A terminal does: it logs in, switches user, and re-authenticates on token loss,
// all on a session it owns exclusively. The web frontend does NOT — the gateway
// authenticates before the App is built, and the session is SHARED across the
// user's tabs, so any in-App re-authentication would mutate a connection other
// tabs are using and could re-key it to a different user (ADR-0061 §2.4; lector
// r3). In the web frontend authentication is not the App's job, and a lost session
// is terminal.
func (m *Model) managesOwnAuth() bool { return m.frontend == FrontendTerminal }

// ownsConnection reports whether this frontend owns its RPC connection's
// lifecycle — connecting it, reconnecting on loss, restarting the daemon behind
// it. A terminal owns a connection no one else uses. The web frontend does NOT:
// the gateway dials the connection and shares it across the user's tabs, so an
// App that connected or reconnected it on its own would replace the client every
// other tab is using and advance the shared generation, invalidating their
// in-flight work (ADR-0061 §2.3; lector r4). In the web frontend the connection
// arrives ready and a loss is terminal.
func (m *Model) ownsConnection() bool { return m.frontend == FrontendTerminal }

type aboutView struct {
	widget.Base
	model *Model
	rows  [][2]string
	float *widget.Float
}

func (m *Model) aboutRows() [][2]string {
	info := m.about
	pid, addr := m.session.ServerStatus()
	backend := addr
	switch {
	case addr == "":
		backend = "not connected (" + m.session.addr + " configured)"
	case pid > 0:
		backend = "[PID:" + strconv.FormatInt(pid, 10) + "] " + addr
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
	if bv := m.session.ServerVersion(); bv != "" {
		rows = append(rows, [2]string{"backend build", backendBuildLine(info.Version, bv, m.canRestartDaemon())})
	}
	return append(rows,
		[2]string{"meta store", meta},
		[2]string{"notes", m.notesLine(info)},
		[2]string{"config", cfg},
	)
}

// notesLine reports the root this session ACTUALLY reads, which is not the
// configured base.
//
// Before sign-in there is no identity and therefore no personal root, so About
// says so rather than naming the base: showing `<base>` would name a directory
// this session never reads and would suggest the ownerless tree is still in use
// (ADR-0068 criteria 20-21).
func (m *Model) notesLine(info AboutInfo) string {
	if m.notes != nil {
		return m.notes.Root()
	}
	if info.NotesDir == "" {
		return "(none configured)"
	}
	return "(resolved after sign-in — notes are per user)"
}

// backendBuildLine compares the RUNNING daemon's build against this
// binary's, and says plainly when they differ.
//
// This is the M6 footgun made visible. Twice during manual testing a
// feature "did nothing" because a daemon from an earlier build was still
// serving — the running process kept answering happily while its
// executable had been replaced underneath it. Diagnosing that took
// /proc/<pid>/exe showing "(deleted)". A shared daemon outlives the
// frontend that started it BY DESIGN, so this is not an edge case; it is
// what happens every time you rebuild and forget.
//
// The frontend cannot fix it (restart is the admin's call), so it does
// the one useful thing: name the mismatch and the remedy.
func backendBuildLine(frontend, backend string, canRestart bool) string {
	if frontend == "" {
		frontend = "dev"
	}
	if frontend == backend {
		return backend + " (matches this binary)"
	}
	// The SPC X remedy is only offered to a frontend that has it. Under --web-ui
	// the restart action is withdrawn (§2.7), so pointing a browser user at a key
	// that does not exist would be worse than saying nothing.
	if canRestart {
		return backend + " ≠ " + frontend + " — the running server is a DIFFERENT build. " +
			"Restart it (SPC X) to pick up this one."
	}
	return backend + " ≠ " + frontend + " — the running server is a DIFFERENT build. " +
		"An administrator must restart it from a terminal to pick up this one."
}

func (m *Model) openAbout() {
	v := &aboutView{model: m, rows: m.aboutRows()}
	v.float = m.openFloat("autodb — Enter or Esc to close", v, 78)
}

func (v *aboutView) AcceptsFocus() bool { return true }

func (v *aboutView) Layout(c tui.Constraints) tui.Size {
	return c.Constrain(tui.Size{W: min(c.MaxW, 76), H: min(c.MaxH, len(v.rows))})
}

func (v *aboutView) Render(s tui.Surface) {
	keySt := style.New().Foreground(style.TokenTextMuted)
	valSt := style.New()
	for i, r := range v.rows {
		if i >= s.Size().H {
			break
		}
		if r[0] == "" {
			continue
		}
		drawTo(s, 0, i, r[0], keySt)
		drawTo(s, 13, i, r[1], valSt)
	}
}

// Enter or Esc closes; the float handles Esc itself.
func (v *aboutView) HandleEvent(ev tui.Event) bool {
	if k, ok := ev.(tui.KeyEvent); ok && k.Kind != tui.KeyRelease && k.Code == tui.KeyEnter {
		v.float.Hide()
		return true
	}
	return false
}

func (v *aboutView) Add(...tui.Component) {}
func (v *aboutView) Remove(tui.Component) {}
func (v *aboutView) Children() iter.Seq[tui.Component] {
	return func(func(tui.Component) bool) {}
}

func (v *aboutView) hints() []keyHint {
	return []keyHint{{"Enter", "close"}, {"Esc", "close"}}
}

var _ tui.Container = (*aboutView)(nil)

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
