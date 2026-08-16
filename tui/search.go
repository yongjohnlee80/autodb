package tui

import (
	"strings"

	"github.com/yongjohnlee80/golib/tui/widget"
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
		[]formField{field("pattern")}, func(v []string) (bool, string) {
			q := strings.TrimSpace(v[0])
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

// applyCursorStyles paints the focused panel's cursor cyan and every other
// panel's gray (Johno, M6 manual testing) — the widgets cannot see focus
// that rests on their delegating wrapper, so the Model tells them.
func (m *Model) applyCursorStyles() {
	explorerOn := m.ctx.FocusWithin(m.explorerBox)
	resultsOn := m.ctx.FocusWithin(m.resultsBox)
	if explorerOn != m.explorerFocused {
		m.explorerFocused = explorerOn
		m.explorer.tree.SetStyles(widget.ListStyles{CursorRow: cursorStyle(explorerOn)})
	}
	if resultsOn != m.resultsFocused {
		m.resultsFocused = resultsOn
		if m.results.rawList != nil {
			m.results.rawList.SetStyles(widget.ListStyles{CursorRow: cursorStyle(resultsOn)})
		}
	}
}
