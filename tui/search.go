package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/yongjohnlee80/golib/tui"
)

// In-panel search (Johno, M6 manual testing): `/` prompts for a pattern,
// `n` / `N` walk the matches — the vim vocabulary, in whichever panel has
// focus. Matching is case-insensitive substring over what the panel
// SHOWS: explorer node labels, editor lines, rendered result rows.
//
// Every panel exposes the same three operations, so the Model drives them
// uniformly and the status bar reports position the same way everywhere.

// searchTarget is one searchable panel.
type searchTarget interface {
	// rows returns the searchable text, in display order.
	rows() []string
	// cursor reports the current row.
	cursor() int
	// reveal moves the panel's cursor to a row.
	reveal(i int)
	// name labels the panel in status messages.
	name() string
}

// --- panel adapters -----------------------------------------------------------

type explorerSearch struct{ e *explorer }

func (s explorerSearch) name() string { return "explorer" }
func (s explorerSearch) rows() []string {
	nodes := s.e.tree.VisibleRows()
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Label()
	}
	return out
}
func (s explorerSearch) cursor() int  { return s.e.tree.Cursor() }
func (s explorerSearch) reveal(i int) { s.e.tree.SetCursor(i) }

type editorSearch struct{ m *Model }

func (s editorSearch) name() string   { return "query" }
func (s editorSearch) rows() []string { return s.m.editor.Lines() }
func (s editorSearch) cursor() int    { row, _ := s.m.editor.Line(); return row }
func (s editorSearch) reveal(i int) {
	// Land on the match itself, not just its line.
	col := 0
	if lines := s.m.editor.Lines(); i < len(lines) && s.m.searchQuery != "" {
		if at := strings.Index(strings.ToLower(lines[i]),
			strings.ToLower(s.m.searchQuery)); at >= 0 {
			col = at
		}
	}
	s.m.editor.SetLine(i, col)
}

type resultsSearch struct{ p *resultsPanel }

func (s resultsSearch) name() string { return "results" }
func (s resultsSearch) rows() []string {
	if s.p.res == nil {
		return nil
	}
	out := make([]string, len(s.p.res.Rows))
	for i, row := range s.p.res.Rows {
		cells := make([]string, len(row))
		for j, v := range row {
			cells[j] = renderCell(v)
		}
		out[i] = strings.Join(cells, " ")
	}
	return out
}
func (s resultsSearch) cursor() int {
	if s.p.rawList == nil {
		return 0
	}
	i, _ := s.p.rawList.Selected()
	return i
}
func (s resultsSearch) reveal(i int) {
	if s.p.rawList != nil {
		s.p.rawList.SetCursor(i)
	}
}

// searchPanel resolves the focused panel, or nil when focus is elsewhere
// (a modal float owns its own keys).
func (m *Model) searchPanel() searchTarget {
	switch {
	case m.ctx.FocusWithin(m.explorerBox):
		return explorerSearch{m.explorer}
	case m.ctx.FocusWithin(m.resultsBox):
		return resultsSearch{m.results}
	case m.ctx.FocusWithin(m.editorBox):
		return editorSearch{m}
	}
	return nil
}

// openSearch prompts for a pattern and jumps to the first match.
func (m *Model) openSearch() {
	target := m.searchPanel()
	if target == nil {
		return
	}
	m.openForm("search in "+target.name()+" — n: next, N: previous",
		[]formField{field("pattern")}, func(v formValues) (bool, string) {
			q := v.str(0)
			if q == "" {
				return false, "pattern required"
			}
			m.searchQuery = q
			m.jumpMatch(target, +1, true)
			return true, ""
		})
}

// searchNext walks matches in the focused panel (n / N).
func (m *Model) searchNext(dir int) {
	if m.searchQuery == "" {
		m.setStatus("no search pattern — / starts one")
		return
	}
	target := m.searchPanel()
	if target == nil {
		return
	}
	m.jumpMatch(target, dir, false)
}

// jumpMatch moves to the next match in dir, wrapping. fromCursor starts AT
// the cursor (a fresh search should match where you already are) rather
// than after it.
func (m *Model) jumpMatch(target searchTarget, dir int, includeCursor bool) {
	rows := target.rows()
	if len(rows) == 0 {
		m.setStatus("nothing to search in the " + target.name())
		return
	}
	needle := strings.ToLower(m.searchQuery)
	var hits []int
	for i, r := range rows {
		if strings.Contains(strings.ToLower(r), needle) {
			hits = append(hits, i)
		}
	}
	if len(hits) == 0 {
		m.setStatus("no match for " + m.searchQuery + " in the " + target.name())
		return
	}
	cur := target.cursor()
	next := -1
	if dir > 0 {
		for _, h := range hits {
			if h > cur || (includeCursor && h >= cur) {
				next = h
				break
			}
		}
		if next < 0 {
			next = hits[0] // wrap
		}
	} else {
		for i := len(hits) - 1; i >= 0; i-- {
			if hits[i] < cur {
				next = hits[i]
				break
			}
		}
		if next < 0 {
			next = hits[len(hits)-1] // wrap
		}
	}
	target.reveal(next)
	at := 1
	for i, h := range hits {
		if h == next {
			at = i + 1
			break
		}
	}
	m.setStatus(m.searchQuery + ": match " + itoa(at) + "/" + itoa(len(hits)) +
		" in the " + target.name())
	// The explorer tracks the active connection from its cursor.
	if es, ok := target.(explorerSearch); ok {
		if n, sel := es.e.tree.Selected(); sel {
			m.noteConnFromNode(n.ID())
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// --- focus-dependent cursor styling -------------------------------------------

// applyCursorStyles paints the focused panel's cursor cyan on black and every
// other panel's cyan on gray — the widgets cannot see focus that rests on
// their delegating wrapper, so the Model tells them.
//
// IT SENDS THE WHOLE ListStyles, and that is the correction. It used to send
// widget.ListStyles{CursorRow: …} and nothing else. SetStyles keeps a field
// whose replacement is the zero value, so the other three — including
// CursorSelected, which is what these lists actually render, the row under the
// cursor being also the selected row — kept whatever they held. The panels
// were built with a complete style and then had one quarter of it overwritten
// on every focus change, so a focused explorer went on wearing the look of a
// blurred one. Fixing the CONSTRUCTORS was not enough: this is the other entry
// point, and it is the one that runs on every keystroke that moves focus.
//
// THE FIRST CALL ALWAYS PAINTS. The transition guard below is what keeps this
// cheap, and on its own it also means that when the initial focus state
// happens to match the zero value of the tracking field, nothing is ever sent
// and the panels keep whatever their constructors guessed.
func (m *Model) applyCursorStyles() {
	if m.ctx == nil {
		return
	}
	live := m.livePane()
	m.explorer.tree.SetStyles(listStyles(live == tui.Component(m.explorer)))
	if m.results.rawList != nil {
		m.results.rawList.SetStyles(listStyles(live == tui.Component(m.results)))
	}
	m.traceFocus(live)
}

// livePane is the pane the operator's KEYS REACH, which is not always the pane
// the framework calls focused.
//
// MEASURED, NOT ASSUMED. A trace taken from the running binary on a production
// host recorded 25 of 36 repaints reporting that NO pane held focus -- and the
// ordering showed why: a correct reading arrives when focus lands and is
// overwritten within the same millisecond by several readings of "nothing is
// focused". The explorer rebuilds its tree on connect, which mutates the node
// tree and leaves the app's focused node cleared, and nothing re-establishes
// it. Typing goes on working because this Model routes keys by its own
// remembered pane rather than by framework focus, so the app never appeared
// broken -- only the colours did, and they were telling the truth about a
// framework state nobody else consulted.
//
// So the accent followed framework focus into nowhere, every pane was painted
// blurred, and the operator navigating the explorer watched a gray cursor move.
// The fix is to paint what the keys do: if the framework names a pane, that is
// the live one; if it names none, the live one is the pane this Model will
// route the next keystroke to.
func (m *Model) livePane() tui.Component {
	for _, pane := range []tui.Component{m.explorer, m.editor, m.results} {
		if pane != nil && m.ctx.FocusWithin(pane) {
			return pane
		}
	}
	return m.lastPane
}

// traceFocus records what the framework says about focus and what was painted
// because of it, when AUTODB_FOCUS_TRACE names a file.
//
// THE INSTRUMENT THAT WAS MISSING. On a live terminal the accent lands on the
// pane the keyboard is not in; in the test backend it does not, and every cell
// written against this was written against the test backend. So the harness
// cannot be the witness -- this reads the same values out of the RUNNING
// binary, on the machine where the symptom is.
//
// Off unless the variable is set, appended rather than truncated, and failures
// are swallowed: a diagnostic that can break the application it is diagnosing
// is not one anybody will turn on.
func (m *Model) traceFocus(live tui.Component) {
	path := os.Getenv("AUTODB_FOCUS_TRACE")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	// THE BOXES AND THE PANES ARE ASKED SEPARATELY, because the contradiction
	// this has to settle is that keys reach a pane while FocusWithin reports
	// nothing for any of them. Either the focused node is not under the BOX
	// this code asks about -- in which case asking the pane directly answers
	// true and the query was simply aimed at the wrong component -- or focus
	// really is nowhere, and the keys are arriving by a route that does not go
	// through framework focus at all. Those need different fixes, and no
	// amount of reading the code has distinguished them.
	fmt.Fprintf(f, "%s box[e=%v q=%v r=%v] pane[e=%v q=%v r=%v] host=%v last=%q painted=%q\n",
		time.Now().Format("15:04:05.000"),
		m.ctx.FocusWithin(m.explorerBox), m.ctx.FocusWithin(m.editorBox), m.ctx.FocusWithin(m.resultsBox),
		m.ctx.FocusWithin(m.explorer), m.ctx.FocusWithin(m.editor), m.ctx.FocusWithin(m.results),
		m.ctx.FocusWithin(m.host),
		m.paneName(m.lastPane), m.paneName(live))
}

// paneName names a pane for the trace.
func (m *Model) paneName(c tui.Component) string {
	switch {
	case c == nil:
		return "none"
	case c == tui.Component(m.explorer):
		return "explorer"
	case c == tui.Component(m.editor):
		return "editor"
	case c == tui.Component(m.results):
		return "results"
	}
	return "other"
}

// lastPaneName names the pane the model believes the keyboard is in, for the
// trace. It is the model's own opinion, deliberately: if it disagrees with the
// FocusWithin answers beside it, the trace has caught the disagreement.
func (m *Model) lastPaneName() string {
	switch {
	case m.ctx.FocusWithin(m.explorerBox):
		return "explorer"
	case m.ctx.FocusWithin(m.editorBox):
		return "editor"
	case m.ctx.FocusWithin(m.resultsBox):
		return "results"
	}
	return "none"
}
