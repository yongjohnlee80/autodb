package autodocplugin

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodoc/plugin"
)

// THE CARD — what each step shows and the keys it takes.
//
//	finding    "Looking for AutoDB…"
//	install    where it looked; i downloads the release matching this plugin, checked by its .sha256
//	starting   "Starting AutoDB…" while the server is reached or started
//	login      name and passphrase; Tab moves between them, Enter signs in
//	pick       the connections; ↑↓ (j k) choose, Enter connects
//	ready      the connection, the last result, and the SPC p commands
//	failed     why the server could not be reached; Enter tries again
//
// A problem (red) and the last thing done stay at the bottom until the next step.

// Key is a key typed into the focused card.
func (p *Plugin) Key(k plugin.Key) {
	switch p.step {
	case stepInstall:
		if k.Key == "i" || k.Key == "Enter" {
			p.doInstall()
		}
	case stepFailed:
		if k.Key == "Enter" {
			p.boot()
		}
	case stepLogin:
		p.loginKey(k)
	case stepPick:
		p.pickKey(k)
	case stepReady:
		if k.Key == "c" {
			p.choose()
		}
	}
}

func (p *Plugin) loginKey(k plugin.Key) {
	f := &p.name
	if p.field == 1 {
		f = &p.pass
	}
	switch {
	case k.Key == "Enter":
		if p.field == 0 {
			p.field = 1
			p.draw()
			return
		}
		p.login()
		return
	case k.Key == "Tab" || k.Key == "Up" || k.Key == "Down":
		p.field = 1 - p.field
	case k.Key == "Backspace":
		if n := len(*f); n > 0 {
			(*f)[n-1] = 0
			*f = (*f)[:n-1]
		}
	case k.Ctrl && k.Key == "u":
		clear(*f)
		*f = (*f)[:0]
	case !k.Ctrl && !k.Alt && k.Text != "":
		*f = append(*f, []rune(k.Text)...)
	default:
		return
	}
	p.draw()
}

func (p *Plugin) pickKey(k plugin.Key) {
	switch k.Key {
	case "Up", "k":
		p.pick = max(p.pick-1, 0)
	case "Down", "j":
		p.pick = min(p.pick+1, max(len(p.conns)-1, 0))
	case "Enter":
		if p.pick < len(p.conns) {
			p.use(p.conns[p.pick])
		}
		return
	default:
		return
	}
	p.draw()
}

// Styles, in the themes' vocabulary.
var (
	stTitle   = plugin.Style{Bold: true}
	stPlain   = plugin.Style{}
	stDim     = plugin.Style{FG: "gray"}
	stProblem = plugin.Style{FG: "red"}
	stGood    = plugin.Style{FG: "green"}
	stField   = plugin.Style{Underline: true}
	stChosen  = plugin.Style{Bold: true, FG: "cyan"}
)

type line struct {
	text string
	st   plugin.Style
}

// lines is what the card shows now, top to bottom, before wrapping.
func (p *Plugin) lines() (top, bottom []line) {
	add := func(st plugin.Style, format string, a ...any) { top = append(top, line{fmt.Sprintf(format, a...), st}) }
	add(stTitle, "AutoDB")
	switch p.step {
	case stepFinding:
		add(stPlain, "Looking for AutoDB…")
	case stepInstall:
		add(stPlain, "AutoDB was not found. Looked in:")
		for _, t := range p.tried {
			where := t.Path
			if where == "" {
				where = "-"
			}
			add(stDim, "  %s: %s (%s)", t.Via, where, t.Why)
		}
		if autodb.IsRelease(p.version) {
			r := autodb.ThisPlatform(p.version)
			add(stPlain, "")
			add(stPlain, "Press i to download AutoDB %s for %s/%s, checked against its .sha256, into %s.",
				p.version, r.OS, r.Arch, autodb.InstallPath(p.pluginDir))
			add(stDim, "  %s", r.URL())
		}
	case stepStarting:
		add(stPlain, "Starting AutoDB…")
		add(stDim, "  %s (%s, %s)", p.bin.Path, p.bin.Via, p.bin.Version)
	case stepLogin:
		p.serverLine(add)
		if p.noUsers {
			add(stProblem, "AutoDB has no user yet: create the first administrator with `autodb --init` (or in `autodb --ui`), then sign in here.")
		}
		add(stPlain, "")
		add(stPlain, "Sign in")
		nameSt, passSt := stField, stPlain
		if p.field == 1 {
			nameSt, passSt = stPlain, stField
		}
		top = append(top, line{"  name:       " + string(p.name) + cursorMark(p.field == 0), nameSt})
		top = append(top, line{"  passphrase: " + strings.Repeat("*", len(p.pass)) + cursorMark(p.field == 1), passSt})
		add(stDim, "Tab: the other field · Enter: sign in")
	case stepPick:
		p.serverLine(add)
		add(stPlain, "")
		add(stPlain, "Choose a connection")
		for i, c := range p.conns {
			mark, st := "  ", stPlain
			if i == p.pick {
				mark, st = "> ", stChosen
			}
			top = append(top, line{fmt.Sprintf("%s%s (%s)", mark, c.Name, c.Engine), st})
		}
		add(stDim, "↑↓ or j k: choose · Enter: connect")
	case stepReady:
		p.serverLine(add)
		top = append(top, line{"connected to " + p.conn.Name + " (" + p.conn.Engine + ")", stGood})
		add(stPlain, "")
		add(stDim, "SPC p r  run the statement or the selection")
		add(stDim, "SPC p s  run the whole note")
		add(stDim, "SPC p x  cancel the query")
		add(stDim, "SPC p c  or c here: another connection")
	case stepFailed:
		add(stPlain, "AutoDB could not be reached.")
		add(stDim, "Enter: try again")
	}
	if p.problem != "" {
		bottom = append(bottom, line{p.problem, stProblem})
	}
	if p.status != "" {
		bottom = append(bottom, line{p.status, stPlain})
	}
	return top, bottom
}

func (p *Plugin) serverLine(add func(plugin.Style, string, ...any)) {
	if p.cli != nil {
		add(stDim, "AutoDB %s at %s", p.cli.Hello.Version, p.cli.Addr)
	}
}

func cursorMark(on bool) string {
	if on {
		return "_"
	}
	return ""
}

// draw sends the card as it stands.
func (p *Plugin) draw() {
	if p.host == nil || p.w <= 0 || p.h <= 0 {
		return
	}
	f := plugin.NewFrame(p.w, p.h)
	top, bottom := p.lines()
	var rows []line
	for _, l := range top {
		rows = append(rows, wrap(l, p.w-2)...)
	}
	var tail []line
	for _, l := range bottom {
		tail = append(tail, wrap(l, p.w-2)...)
	}
	// the bottom lines are the newest news: they keep their rows, the top gives way
	if len(tail) > p.h {
		tail = tail[len(tail)-p.h:]
	}
	room := max(p.h-len(tail), 0)
	if len(rows) > room {
		rows = rows[:room]
	}
	for y, l := range rows {
		f.Text(1, y, l.text, l.st)
	}
	for i, l := range tail {
		f.Text(1, p.h-len(tail)+i, l.text, l.st)
	}
	_ = p.host.Frame(f)
}

// wrap breaks a line at spaces to width cells (one a character), a word longer than width cut.
func wrap(l line, width int) []line {
	if width <= 0 {
		return nil
	}
	var out []line
	var cur strings.Builder
	n := 0
	flush := func() {
		out = append(out, line{cur.String(), l.st})
		cur.Reset()
		n = 0
	}
	for i, word := range strings.Split(l.text, " ") {
		wn := utf8.RuneCountInString(word)
		if i > 0 {
			if n > 0 && n+1+wn > width {
				flush()
			} else {
				cur.WriteByte(' ')
				n++
			}
		}
		for wn > width-n {
			r := []rune(word)
			cur.WriteString(string(r[:width-n]))
			word = string(r[width-n:])
			wn = utf8.RuneCountInString(word)
			flush()
		}
		cur.WriteString(word)
		n += wn
	}
	flush()
	return out
}
