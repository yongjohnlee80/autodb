package tui

import (
	"fmt"
	"strings"

	tuicore "github.com/yongjohnlee80/golib/tui"
)

// Search stays in the host: the query/editor and result model are data, not
// QML policy. It intentionally moves a caret or result row, as the legacy
// search did; there is no claim of highlighting a full match substring.
type searchState struct {
	query, target                 string // query, table, json
	identity, resultSeq, queryRev uint64
	json                          bool
	row                           int
}

func (h *Host) resultsMoved(row int) error { h.results.cursor = row; return nil }

func (h *Host) openSearch() error {
	switch h.paneWithFocus() {
	case paneEditor:
		h.search.target = "query"
	case paneResults:
		if h.results.last == nil {
			h.setStatus("nothing to search in the results")
			return nil
		}
		h.search.target = "table"
		if h.results.asJSON {
			h.search.target = "json"
		}
	default:
		h.setStatus("focus the query or results pane before searching")
		return nil
	}
	h.set("App.searchTitle", "find in "+h.search.target+" — n next, N previous")
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
	if h.search.target == "" {
		h.setStatus("search target changed — press / again")
		return nil
	}
	h.search.query = pattern
	h.search.identity = h.session.IdentityEpoch()
	h.search.resultSeq = h.results.seq
	h.search.queryRev = h.searchQueryRev
	h.search.json = h.results.asJSON
	h.set("App.lastSearch", pattern)
	h.p.Post(func() { h.searchJump(+1, true) }) // after Search's modal closes
	return nil
}

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
	if s.identity != h.session.IdentityEpoch() ||
		(s.target == "query" && s.queryRev != h.searchQueryRev) ||
		(s.target != "query" && (s.resultSeq != h.results.seq || s.json != h.results.asJSON)) {
		s.target = ""
		h.setStatus("search document changed — / starts again")
		return
	}
	if !includeCurrent {
		pane := h.paneWithFocus()
		if (s.target == "query" && pane != paneEditor) || (s.target != "query" && pane != paneResults) {
			h.setStatus("focus the original search pane or press / for a new one")
			return
		}
	}
	var rows []string
	cur := 0
	switch s.target {
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
