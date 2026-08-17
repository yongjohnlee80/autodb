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
		rows = append(rows, [2]string{"backend build", backendBuildLine(info.Version, bv)})
	}
	return append(rows,
		[2]string{"meta store", meta},
		[2]string{"notes", info.NotesDir},
		[2]string{"config", cfg},
	)
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
func backendBuildLine(frontend, backend string) string {
	if frontend == "" {
		frontend = "dev"
	}
	if frontend == backend {
		return backend + " (matches this binary)"
	}
	return backend + " ≠ " + frontend + " — the running server is a DIFFERENT build. " +
		"Restart it (SPC X) to pick up this one."
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
