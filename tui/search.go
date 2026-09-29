package tui

import (
	"fmt"
	"slices"
	"strings"

	tuicore "github.com/yongjohnlee80/golib/tui"
	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
)

// Search stays in the host: the explorer, query/editor and result model are
// data, not QML policy. It intentionally moves a cursor, caret or result row,
// as the legacy search did; there is no claim of highlighting a full match
// substring.
//
// THE EXPLORER is searched over every row the host has LOADED, in the order
// the tree shows them, including rows under a folder that is closed now: a
// hit there is revealed, its folders opened (TreeView.setCurrentIndex). Rows
// not loaded yet are not searched; opening a folder loads it, and the next
// search sees it. A hit moves the cursor only: as with the arrow keys, the
// query's connection changes on Enter, not on arrival.
type searchState struct {
	query, target                 string // query, table, json
	identity, resultSeq, queryRev uint64
	json                          bool
	row                           int
}

func (h *Host) resultsMoved(row int) error { h.results.cursor = row; return nil }

func (h *Host) openSearch() error {
	var target string
	switch h.paneWithFocus() {
	case paneExplorer:
		target = "explorer"
	case paneEditor:
		target = "query"
	case paneResults:
		if h.results.last == nil {
			h.setStatus("nothing to search in the results")
			return nil
		}
		target = "table"
		if h.results.asJSON {
			target = "json"
		}
	default:
		h.setStatus("focus the explorer, query or results pane before searching")
		return nil
	}
	h.searchPending = target
	h.set("App.searchTitle", "find in "+target+" — n next, N previous")
	h.set("App.searchError", "")
	h.open("search")
	return nil
}

func (h *Host) startSearch(pattern string) error {
	if strings.TrimSpace(pattern) == "" {
		h.set("App.searchError", "pattern required")
		h.p.Post(func() { h.open("search") })
		return nil
	}
	if h.searchPending == "" {
		h.setStatus("search target changed — press / again")
		return nil
	}
	h.search.target, h.searchPending = h.searchPending, ""
	h.search.query = pattern
	h.search.identity = h.session.IdentityEpoch()
	h.search.resultSeq = h.results.seq
	h.search.queryRev = h.searchQueryRev
	h.search.json = h.results.asJSON
	h.set("App.lastSearch", pattern)
	h.p.Post(func() { h.searchJump(+1, true) }) // after Search's modal closes
	return nil
}

func (h *Host) searchCancelled() error { h.searchPending = ""; return nil }

func (h *Host) searchNext() error     { h.searchJump(+1, false); return nil }
func (h *Host) searchPrevious() error { h.searchJump(-1, false); return nil }

func (h *Host) invalidateResultsSearch() {
	if h.search.target == "table" || h.search.target == "json" {
		h.search.target = ""
	}
}

func (h *Host) invalidateQuerySearch() {
	h.searchQueryRev++
	if h.search.target == "query" {
		h.search.target = ""
	}
}

func (h *Host) searchJump(dir int, includeCurrent bool) {
	s := &h.search
	if s.query == "" || s.target == "" {
		h.setStatus("no current search — / starts one")
		return
	}
	// The explorer's rows are read afresh on every jump, so only a new
	// identity makes its search stale.
	if s.identity != h.session.IdentityEpoch() ||
		(s.target == "query" && s.queryRev != h.searchQueryRev) ||
		(s.target == "table" || s.target == "json") && (s.resultSeq != h.results.seq || s.json != h.results.asJSON) {
		s.target = ""
		h.setStatus("search document changed — / starts again")
		return
	}
	if !includeCurrent {
		pane := h.paneWithFocus()
		want := map[string]string{"explorer": paneExplorer, "query": paneEditor}[s.target]
		if want == "" {
			want = paneResults
		}
		if pane != want {
			h.setStatus("focus the original search pane or press / for a new one")
			return
		}
	}
	var rows []string
	var at []tuidecl.Index
	cur := 0
	switch s.target {
	case "explorer":
		rows, at = h.explorerRows()
		cur = -1 // before the first row, unless the cursor is on one
		for i, ix := range at {
			if slices.Equal(h.explorerKeys(ix), h.explorerAt) {
				cur = i
				break
			}
		}
	case "query":
		rows = h.editor.Lines()
		cur, _ = h.editor.Line()
	case "json":
		rows = h.jsonEditor.Lines()
		cur, _ = h.jsonEditor.Line()
	case "table":
		cur = h.results.cursor
		if h.results.last != nil {
			for _, record := range h.results.last.Rows {
				var cells []string
				for _, cell := range record {
					cells = append(cells, renderCell(cell))
				}
				rows = append(rows, strings.Join(cells, "  "))
			}
		}
	default:
		return
	}
	if len(rows) == 0 {
		h.setStatus("nothing to search in the " + s.target)
		return
	}
	var hits []int
	cols := map[int]int{}
	for i, row := range rows {
		if col, found := clusterMatch(row, s.query); found {
			hits = append(hits, i)
			cols[i] = col
		}
	}
	if len(hits) == 0 {
		h.setStatus("no match for " + s.query + " in the " + s.target)
		return
	}
	selected := 0
	if dir > 0 {
		for i, row := range hits {
			if row > cur || (includeCurrent && row >= cur) {
				selected = i
				break
			}
		}
	} else {
		selected = len(hits) - 1
		for i := len(hits) - 1; i >= 0; i-- {
			if hits[i] < cur {
				selected = i
				break
			}
		}
	}
	row := hits[selected]
	s.row = row
	switch s.target {
	case "explorer":
		if err := h.p.Call(paneExplorer, "setCurrentIndex", at[row]); err != nil {
			h.keep(err)
			return
		}
		h.explorerAt = h.explorerKeys(at[row])
		h.focusPane(paneExplorer)
	case "query":
		h.editor.SetLine(row, cols[row])
		h.focusEditor()
	case "json":
		h.jsonEditor.SetLine(row, cols[row])
		h.focusPane(paneResults)
	case "table":
		h.results.cursor = row
		h.set("App.resultsIndex", row)
		h.focusPane(paneResults)
	}
	h.setStatus(fmt.Sprintf("%s: match %d/%d in the %s", s.query, selected+1, len(hits), s.target))
}

// Editor.SetLine uses GRAPHEME columns, not byte offsets. EqualFold compares
// candidate clusters without ever passing a byte position into the widget.
func clusterMatch(line, pattern string) (int, bool) {
	var hay, needle []string
	for c := range tuicore.Graphemes(line) {
		hay = append(hay, c)
	}
	for c := range tuicore.Graphemes(pattern) {
		needle = append(needle, c)
	}
	if len(needle) == 0 || len(needle) > len(hay) {
		return 0, false
	}
	for start := 0; start+len(needle) <= len(hay); start++ {
		if strings.EqualFold(strings.Join(hay[start:start+len(needle)], ""), pattern) {
			return start, true
		}
	}
	return 0, false
}

// explorerMoved is App.explorerMoved(index): the row now under the explorer's
// cursor, kept as its path of keys.
func (h *Host) explorerMoved(ix tuidecl.Index) error {
	h.explorerAt = h.explorerKeys(ix)
	return nil
}

// explorerKeys is a row's identity in the tree: the keys from the top row
// down to it. A key alone is not enough; a connection attached to two
// workspaces shows its schemas under both.
func (h *Host) explorerKeys(ix tuidecl.Index) []string {
	var keys []string
	for p := &ix; p != nil; p = p.Parent {
		keys = append([]string{h.explorer.model.Key(*p)}, keys...)
	}
	return keys
}

// explorerRows are the explorer's loaded rows in the order the tree shows
// them, with each row's Index: a row's children follow it when they are
// loaded, whether or not its folder is open now. Only loaded children are
// walked, which is also what setCurrentIndex needs to reach a row.
func (h *Host) explorerRows() ([]string, []tuidecl.Index) {
	m := h.explorer.model
	var labels []string
	var at []tuidecl.Index
	var walk func(parent *tuidecl.Index)
	walk = func(parent *tuidecl.Index) {
		for r := 0; r < m.RowCount(parent); r++ {
			ix := tuidecl.Index{Row: r, Parent: parent}
			labels = append(labels, m.Data(ix, "label").Raw)
			at = append(at, ix)
			if m.RowCount(&ix) > 0 && !m.CanFetchMore(ix) {
				walk(&ix)
			}
		}
	}
	walk(nil)
	return labels, at
}
