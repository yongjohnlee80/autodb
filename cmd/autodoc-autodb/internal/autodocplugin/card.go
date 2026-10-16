package autodocplugin

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/yongjohnlee80/autodb/cmd/autodoc-autodb/internal/autodb"
	"github.com/yongjohnlee80/autodoc/plugin"
)

// THE CARD — what each step shows and the keys it takes. Each step is laid out as AutoDoc's own
// dialogs are: a heading, the content, a rule, the buttons (the sign-in's), the keys on a line of
// their own, then what was done last and what went wrong, in the error style. Once a connection is
// picked, every step's first line names it, and so do the card's title and the results' title.
//
//	finding    "Looking for AutoDB…"
//	install    where it looked; i downloads the release matching this plugin, checked by its .sha256
//	starting   "Starting AutoDB…" while the server is reached or started
//	login      the name and the passphrase (masked), and Sign in and Cancel: Tab and Shift+Tab move
//	           between them, Enter activates
//	pick       the connections; ↑↓ (j k) choose, Enter connects
//	ready      the connection, the last result, and the SPC p commands
//	failed     why the server could not be reached; Enter tries again
//
// q closes the card wherever no text field has the keys: on the sign-in's fields it is a letter.
// AutoDoc sends a card keys only while the card has them, so a key here is the card's.

// The sign-in's controls, in Tab order: p.field is the one focused.
const (
	focusName = iota
	focusPass
	focusSignIn
	focusCancel
	focusCount
)

// Key is a key typed into the focused card.
func (p *Plugin) Key(k plugin.Key) {
	if p.step == stepLogin {
		p.loginKey(k)
		return
	}
	if isQ(k) {
		p.closeCard()
		return
	}
	switch p.step {
	case stepInstall:
		if k.Key == "i" || k.Key == "Enter" {
			p.doInstall()
		}
	case stepFailed:
		if k.Key == "Enter" {
			p.boot()
		}
	case stepPick:
		p.pickKey(k)
	case stepReady:
		if k.Key == "c" {
			p.choose()
		}
	}
}

// isQ is q typed alone.
func isQ(k plugin.Key) bool { return k.Key == "q" && !k.Ctrl && !k.Alt }

// closeCard asks AutoDoc to close the card. A passphrase half typed is dropped first: the plugin
// may run on unseen.
func (p *Plugin) closeCard() {
	p.dropPass()
	p.draw()
	if p.host != nil {
		_ = p.host.Close()
	}
}

// dropPass forgets the passphrase typed.
func (p *Plugin) dropPass() {
	clear(p.pass)
	p.pass = p.pass[:0]
}

func (p *Plugin) loginKey(k plugin.Key) {
	switch {
	case k.Key == "Tab" && k.Shift, k.Key == "Up":
		p.field = (p.field + focusCount - 1) % focusCount
	case k.Key == "Tab", k.Key == "Down":
		p.field = (p.field + 1) % focusCount
	case p.field == focusSignIn || p.field == focusCancel:
		p.buttonKey(k)
		return
	default:
		p.fieldKey(k)
		return
	}
	p.draw()
}

// fieldKey is a key typed into the name or the passphrase: every printable key is text, q too.
func (p *Plugin) fieldKey(k plugin.Key) {
	f := &p.name
	if p.field == focusPass {
		f = &p.pass
	}
	switch {
	case k.Key == "Enter":
		if p.field == focusName {
			p.field = focusPass
			p.draw()
			return
		}
		p.login()
		return
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

// buttonKey is a key on Sign in or Cancel: Enter or Space presses it, ← → move between the two,
// and q closes, no field having the keys.
func (p *Plugin) buttonKey(k plugin.Key) {
	switch {
	case k.Key == "Enter" || k.Key == " ":
		if p.field == focusSignIn {
			p.login()
			return
		}
		p.closeCard()
	case isQ(k):
		p.closeCard()
	case k.Key == "Left" || k.Key == "Right":
		p.field = focusSignIn + focusCancel - p.field
		p.draw()
	}
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

// Hide is the card's panel closed while the plugin runs on: the session, the connection and the
// table stay, and a passphrase half typed goes.
func (p *Plugin) Hide() { p.dropPass() }

// Show is the panel open again: the card is drawn as it stands.
func (p *Plugin) Show() { p.draw() }

// Styles, in the themes' vocabulary.
var (
	stTitle   = plugin.Style{Bold: true}
	stPlain   = plugin.Style{}
	stDim     = plugin.Style{FG: "gray"}
	stProblem = plugin.Style{FG: "red"}
	stGood    = plugin.Style{FG: "green"}
	stChosen  = plugin.Style{Bold: true, FG: "cyan"}
	// a field is underlined across its width; the focused one is the chosen colour too
	stField      = plugin.Style{Underline: true}
	stFieldFocus = plugin.Style{Underline: true, Bold: true, FG: "cyan"}
)

// span is text in one style; a row is one line of the card, laid out to its width.
type span struct {
	text string
	st   plugin.Style
}

type row []span

// The marks drawn in a field: the passphrase's characters, the cursor, and the cut when the value
// is wider than the field.
const (
	maskRune   = '•'
	cursorRune = '█'
	elision    = "…"
)

// layout is the card as rows width cells wide: top is the connection, the heading and the
// content, which give way from the bottom when the card is short, except its first fixed rows (the
// connection and the heading) and the row at keep (-1 none); bottom is the rule, the buttons, the
// keys and the news, which keep their rows.
type layout struct {
	width       int
	top, bottom []row
	fixed, keep int
}

// heading is the step's title, the last of the rows that always show.
func (l *layout) heading(text string) {
	l.add(stTitle, "%s", text)
	l.fixed = len(l.top)
}

// para adds text to rows, wrapped at spaces to the width.
func (l *layout) para(to *[]row, st plugin.Style, format string, a ...any) {
	for _, s := range wrap(fmt.Sprintf(format, a...), l.width) {
		*to = append(*to, row{{s, st}})
	}
}

// add is a paragraph of the content.
func (l *layout) add(st plugin.Style, format string, a ...any) { l.para(&l.top, st, format, a...) }

// one adds a row that is never wrapped: text past the width is cut with an ellipsis.
func (l *layout) one(st plugin.Style, s string) { l.top = append(l.top, row{{elide(s, l.width), st}}) }

func (l *layout) blank() { l.top = append(l.top, row{}) }

// lines is what the card shows now, laid out width cells wide.
func (p *Plugin) lines(width int) layout {
	l := layout{width: width, keep: -1}
	if line, st := p.connLine(); line != "" {
		l.add(st, "%s", line)
	}
	var hint string
	var buttons []row
	switch p.step {
	case stepFinding:
		l.heading("AutoDB")
		l.add(stPlain, "Looking for AutoDB…")
		hint = "q: close"
	case stepInstall:
		l.heading("Install AutoDB")
		l.add(stPlain, "AutoDB was not found. Looked in:")
		for _, t := range p.tried {
			where := t.Path
			if where == "" {
				where = "-"
			}
			l.add(stDim, "%s: %s (%s)", t.Via, where, t.Why)
		}
		hint = "q: close"
		if autodb.IsRelease(p.version) {
			r := autodb.ThisPlatform(p.version)
			l.blank()
			l.add(stPlain, "Download AutoDB %s for %s/%s, checked against its .sha256, into %s.",
				p.version, r.OS, r.Arch, autodb.InstallPath(p.pluginDir))
			l.add(stDim, "%s", r.URL())
			hint = "i: install · q: close"
		}
	case stepStarting:
		l.heading("Starting AutoDB")
		l.add(stDim, "%s (%s, %s)", p.bin.Path, p.bin.Via, p.bin.Version)
		hint = "q: close"
	case stepLogin:
		l.heading("Sign in to AutoDB")
		p.serverLine(&l)
		if p.noUsers {
			l.add(stProblem, "AutoDB has no user yet: create the first administrator with `autodb --init` (or in `autodb --ui`), then sign in here.")
		}
		l.blank()
		l.one(stPlain, "name")
		if p.field == focusName {
			l.keep = len(l.top)
		}
		l.top = append(l.top, fieldRow(string(p.name), width, p.field == focusName))
		l.one(stPlain, "passphrase")
		if p.field == focusPass {
			l.keep = len(l.top)
		}
		l.top = append(l.top, fieldRow(strings.Repeat(string(maskRune), len(p.pass)), width, p.field == focusPass))
		buttons = buttonRows(width, p.field-focusSignIn, "Sign in", "Cancel")
		switch p.field {
		case focusName:
			hint = "Tab Shift+Tab: move · Enter: next"
		case focusPass:
			hint = "Tab Shift+Tab: move · Enter: sign in"
		case focusSignIn:
			hint = "Tab: move · Enter: sign in · q: close"
		default:
			hint = "Tab: move · Enter: cancel · q: close"
		}
	case stepPick:
		l.heading("Choose a connection")
		p.serverLine(&l)
		l.blank()
		for i, c := range p.conns {
			mark, st := "  ", stPlain
			if i == p.pick {
				mark, st = "> ", stChosen
				l.keep = len(l.top)
			}
			current := ""
			if p.conn != nil && c.ID == p.conn.ID {
				current = " · current"
			}
			text := elide(fmt.Sprintf("%s%s (%s)", mark, c.Name, c.Engine), width-utf8.RuneCountInString(current))
			l.one(st, text+current)
		}
		hint = "↑↓ j k: choose · Enter: connect · q: close"
	case stepReady:
		l.heading("Ready")
		p.serverLine(&l)
		l.blank()
		l.add(stDim, "SPC p r  run the statement or the selection")
		l.add(stDim, "SPC p s  run the whole note")
		l.add(stDim, "SPC p x  cancel the query")
		l.add(stDim, "SPC p c  or c here: another connection")
		hint = "c: another connection · q: close"
	case stepFailed:
		l.heading("AutoDB is not reachable")
		l.add(stPlain, "AutoDB could not be reached.")
		hint = "Enter: try again · q: close"
	}

	l.bottom = append(l.bottom, row{{strings.Repeat("─", max(width, 0)), stDim}})
	l.bottom = append(l.bottom, buttons...)
	l.para(&l.bottom, stDim, "%s", hint)
	if p.status != "" {
		l.para(&l.bottom, stPlain, "%s", p.status)
	}
	if p.problem != "" {
		l.para(&l.bottom, stProblem, "%s", p.problem)
	}
	return l
}

// connected reports whether the card has a connection to run on: picked, and AutoDB signed in.
func (p *Plugin) connected() bool { return p.conn != nil && p.cli != nil && p.token != "" }

// connLine is the line naming the connection picked, and its style; "" before any pick. A
// connection whose server or sign-in has gone is named as not connected.
func (p *Plugin) connLine() (string, plugin.Style) {
	c := p.conn
	if c == nil {
		return "", stPlain
	}
	if !p.connected() {
		return "Connection: " + c.Name + " (" + c.Engine + ") · not connected", stDim
	}
	s := "Connection: " + c.Name + " (" + c.Engine + ")"
	if c.Workspace != "" {
		s += " · " + c.Workspace
	}
	return s, stGood
}

// cardTitle is the card's title: AutoDB, and the connection picked.
func (p *Plugin) cardTitle() string {
	switch {
	case p.conn == nil:
		return "AutoDB"
	case p.connected():
		return "AutoDB · " + p.conn.Name + " (" + p.conn.Engine + ")"
	default:
		return "AutoDB · " + p.conn.Name + " · not connected"
	}
}

func (p *Plugin) serverLine(l *layout) {
	if p.cli != nil {
		l.add(stDim, "AutoDB %s at %s", p.cli.Hello.Version, p.cli.Addr)
	}
}

// fieldRow is a field width cells wide holding value, underlined across; the focused one shows
// its cursor, and a value too wide for it shows its end.
func fieldRow(value string, width int, focused bool) row {
	if width <= 0 {
		return row{}
	}
	st, cursor := stField, ""
	if focused {
		st, cursor = stFieldFocus, string(cursorRune)
	}
	v := []rune(value + cursor)
	if len(v) > width {
		v = append([]rune(elision), v[len(v)-width+1:]...)
	}
	return row{{string(v) + strings.Repeat(" ", width-len(v)), st}}
}

// buttonRows are the buttons, "[ label ]" each, centred, the one at focused (none when it is out
// of range) in the chosen style. Buttons too wide for one row go on to the next, as words do.
func buttonRows(width, focused int, labels ...string) []row {
	var rows []row
	var cur row
	n := 0
	flush := func() {
		if pad := (width - n) / 2; pad > 0 {
			cur = append(row{{strings.Repeat(" ", pad), stPlain}}, cur...)
		}
		rows = append(rows, cur)
		cur, n = nil, 0
	}
	for i, label := range labels {
		b := "[ " + label + " ]"
		bn := utf8.RuneCountInString(b)
		if n > 0 && n+2+bn > width {
			flush()
		}
		if n > 0 {
			cur = append(cur, span{"  ", stPlain})
			n += 2
		}
		st := stPlain
		if i == focused {
			st = stChosen
		}
		cur = append(cur, span{b, st})
		n += bn
	}
	if len(cur) > 0 {
		flush()
	}
	return rows
}

// draw sends the card as it stands, laid out to the size AutoDoc last gave it, and its title
// when that has changed.
func (p *Plugin) draw() {
	if p.host == nil {
		return
	}
	if t := p.cardTitle(); t != p.titled {
		if p.host.Title(t) == nil {
			p.titled = t
		}
	}
	if p.w <= 0 || p.h <= 0 {
		return
	}
	f := plugin.NewFrame(p.w, p.h)
	l := p.lines(p.w - 2)
	top, tail := l.top, l.bottom
	// the bottom rows are the newest news: they keep their rows, the top gives way
	if len(tail) > p.h {
		tail = tail[len(tail)-p.h:]
	}
	room := max(p.h-len(tail), 0)
	if len(top) > room {
		// the connection and the heading stay, and the rows after them scroll so the focused one
		// shows
		if room > l.fixed && l.keep >= room {
			skip := l.keep - room + 1
			top = append(top[:l.fixed:l.fixed], top[l.fixed+skip:]...)
		}
		top = top[:room]
	}
	for y, r := range top {
		drawRow(f, y, r)
	}
	for i, r := range tail {
		drawRow(f, p.h-len(tail)+i, r)
	}
	_ = p.host.Frame(f)
}

// drawRow writes r at row y, from the card's one-cell margin.
func drawRow(f *plugin.Frame, y int, r row) {
	x := 1
	for _, s := range r {
		x = f.Text(x, y, s.text, s.st)
	}
}

// elide is s cut to width cells, an ellipsis marking the cut.
func elide(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return string(r[:width-1]) + elision
}

// wrap breaks text at spaces to width cells (one a character). A word longer than a whole row is
// the only thing cut: after its last / or - that fits (a path's, a URL's, a file name's), else at
// the row's end.
func wrap(text string, width int) []string {
	if width <= 0 {
		return nil
	}
	var out []string
	var cur strings.Builder
	n := 0
	flush := func() {
		out = append(out, cur.String())
		cur.Reset()
		n = 0
	}
	for i, word := range strings.Split(text, " ") {
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
			cut := width - n
			if at := strings.LastIndexAny(string(r[:cut]), "/-"); at > 0 {
				if k := utf8.RuneCountInString(string(r[:cut])[:at]) + 1; k < cut {
					cut = k
				}
			}
			cur.WriteString(string(r[:cut]))
			word = string(r[cut:])
			wn = utf8.RuneCountInString(word)
			flush()
		}
		cur.WriteString(word)
		n += wn
	}
	flush()
	return out
}
