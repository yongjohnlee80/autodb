package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
)

// RUNNING A QUERY, AND WHAT IT RETURNED.
//
// SPC r runs the visual selection when there is one, the whole buffer
// otherwise; SPC R the selection only. One run at a time. A run carries two
// identities: the connection generation, so a result never crosses a
// reconnect, and its own number, so an older run finishing late cannot replace
// a newer one's result or clear its guard.
//
// The result is a host model the document's TableView shows — its columns are
// the query's, whatever they are — or, with SPC j, the same rows as JSON. A
// summary line sits above either.

// results is the last result and how it is shown.
type results struct {
	model   *tuidecl.ListModel
	last    *ExecResult
	asJSON  bool
	running bool
	seq     uint64
	// run executes a query: the backend's, unless a test holds the answer.
	// answered counts the answers that came back, applied or dropped.
	run      func(ctx context.Context, b *Bound, connID int64, sql string) (*ExecResult, error)
	answered int
}

func newResults() *results {
	return &results{model: tuidecl.NewListModel(), run: func(ctx context.Context, b *Bound, connID int64, sql string) (*ExecResult, error) {
		return b.Run(ctx, connID, sql)
	}}
}

// resultsState are the results pane's sources, with nothing shown.
func resultsState(r *results) map[string]any {
	return map[string]any{
		"App.results":        r.model,
		"App.resultsSummary": "",
		"App.resultsAsTable": false,
		"App.resultsAsJSON":  false,
		"App.resultsJSON":    "",
		"App.resultsEmpty":   true,
	}
}

// runQuery is query.run: the selection, else the buffer.
func (h *Host) runQuery() {
	sql := h.editor.SelectedText()
	if strings.TrimSpace(sql) == "" {
		sql = h.editor.Value()
	}
	h.runSQL(sql)
}

// runSelection is query.run_selection: the selection only.
func (h *Host) runSelection() {
	sql := h.editor.SelectedText()
	if strings.TrimSpace(sql) == "" {
		h.setStatus("no selection — SPC r runs the buffer")
		return
	}
	h.runSQL(sql)
}

func (h *Host) runSQL(sql string) {
	r := h.results
	switch {
	case r.running:
		h.setStatus("a query is already running")
		return
	case h.session.Token() == "":
		h.setStatus("sign in first — SPC L")
		return
	case h.active.id == 0:
		h.setStatus("no connection — Enter on one in the explorer")
		return
	case strings.TrimSpace(sql) == "":
		h.setStatus("the query buffer is empty")
		return
	}
	connID := h.active.id
	r.running = true
	r.seq++
	seq := r.seq
	h.setStatus("running on " + h.connLabel() + "…")
	bound := h.session.Bind() // the generation is pinned when the run is asked for
	type ran struct {
		gen uint64
		res *ExecResult
		err error
	}
	run := r.run
	do(h, func(ctx context.Context) ran {
		res, err := run(ctx, bound, connID, sql)
		return ran{gen: bound.Gen(), res: res, err: err}
	}, func(v ran) {
		r.answered++
		if seq != r.seq {
			return // a newer run, or another identity, owns the pane and the guard
		}
		r.running = false
		switch {
		case v.gen != h.session.Gen():
			h.setStatus("the query was superseded by a reconnect — run it again")
		case v.err != nil:
			h.setStatus(WireErrorMessage(v.err))
		default:
			h.dropInspection() // cards may still show the previous run
			r.last, r.asJSON = v.res, false
			h.showResults()
			h.setStatus(execSummary(v.res))
		}
	})
}

// toggleJSON is results.toggle_json: the rows as a table or as JSON.
func (h *Host) toggleJSON() {
	r := h.results
	if r.last == nil || len(r.last.Columns) == 0 {
		h.setStatus("no rows to show as JSON")
		return
	}
	r.asJSON = !r.asJSON
	h.showResults()
}

// clearResults drops the result: nothing from a server or an identity that is
// gone may keep rendering.
func (h *Host) clearResults() {
	r := h.results
	r.last, r.asJSON = nil, false
	r.seq++ // a run in flight is answering a question nobody asks now
	r.running = false
	h.dropInspection()
	h.showResults()
}

// dropInspection retires the cards and their host/QML values together. A run
// may finish while someone is reading the previous result, not just when a
// session disappears; neither old values nor old column indexes survive it.
func (h *Host) dropInspection() {
	h.inspected = nil
	h.valueText = ""
	h.inspectRows.Reset(nil)
	h.set("App.valueText", "")
	h.set("App.valueTitle", "")
	for _, id := range []string{"value", "inspect"} {
		if err := h.p.Call(id, "close"); err != nil {
			h.keep(err)
		}
	}
}

// showResults sets the pane's sources from the last result.
func (h *Host) showResults() {
	r := h.results
	res := r.last
	state := map[string]any{
		"App.resultsSummary": "",
		"App.resultsAsTable": false,
		"App.resultsAsJSON":  false,
		"App.resultsEmpty":   res == nil,
		"App.resultsJSON":    "",
	}
	if res != nil {
		state["App.resultsSummary"] = execSummary(res)
		if len(res.Columns) > 0 {
			state["App.resultsAsTable"], state["App.resultsAsJSON"] = !r.asJSON, r.asJSON
			if r.asJSON {
				state["App.resultsJSON"] = resultJSON(res)
			}
		}
	}
	h.setTable(res)
	if err := h.p.SetMany(state); err != nil {
		h.keep(err)
	}
}

// setTable puts a result's rows in the table model, one role per column.
func (h *Host) setTable(res *ExecResult) {
	m := h.results.model
	if res == nil || len(res.Columns) == 0 {
		m.SetColumns()
		m.Reset(nil)
		return
	}
	cols := make([]tuidecl.Column, len(res.Columns))
	for i, name := range res.Columns {
		cols[i] = tuidecl.Column{Role: fmt.Sprintf("c%d", i), Title: name}
	}
	rows := make([]tuidecl.Row, len(res.Rows))
	for i, rr := range res.Rows {
		row := tuidecl.Row{}
		for j := range res.Columns {
			if j < len(rr) {
				row[cols[j].Role] = renderCell(rr[j])
			}
		}
		rows[i] = row
	}
	m.SetColumns(cols...)
	m.Reset(rows)
}

// inspectResult opens a result row with every column's full value. The table
// remains a compact summary; the source values, not its rendered cells, back
// both the inspect view and the editor's copy register.
func (h *Host) inspectResult(row int) error {
	res := h.results.last
	if res == nil || row < 0 || row >= len(res.Rows) {
		return nil // a result can change between a table event and this handler
	}
	values := make([]string, len(res.Columns))
	rows := make([]tuidecl.Row, len(res.Columns))
	for col, name := range res.Columns {
		value := "NULL"
		if col < len(res.Rows[row]) {
			value = fullCell(res.Rows[row][col])
		}
		values[col] = value
		rows[col] = tuidecl.Row{"line": name + " = " + renderCell(value)}
	}
	h.inspected = values
	h.inspectRows.Reset(rows)
	h.open("inspect")
	return nil
}

func (h *Host) openValue(col int) error {
	if h.results.last == nil || col < 0 || col >= len(h.inspected) || col >= len(h.results.last.Columns) {
		return nil
	}
	h.set("App.valueTitle", h.results.last.Columns[col])
	h.valueText = h.inspected[col]
	h.set("App.valueText", h.valueText)
	h.open("value")
	return nil
}

func (h *Host) copyInspected(col int) error {
	if col >= 0 && col < len(h.inspected) {
		h.editor.SetRegister(h.inspected[col], false)
		h.setStatus("value copied to the editor register")
	}
	return nil
}

func (h *Host) copyValue() error {
	h.editor.SetRegister(h.valueText, false)
	h.setStatus("value copied to the editor register")
	return nil
}

func fullCell(v any) string {
	if b, ok := v.([]byte); ok {
		return bytesText(b)
	}
	if v == nil {
		return "NULL"
	}
	return fmt.Sprint(v)
}

// execSummary is a result in one line.
func execSummary(res *ExecResult) string {
	if res == nil {
		return "ok"
	}
	prefix := ""
	if res.Statements > 1 {
		prefix = fmt.Sprintf("%d statements ⋅ ", res.Statements)
	}
	if len(res.Columns) == 0 {
		return fmt.Sprintf("%s%s ok — %d row(s) affected in %s", prefix, strings.ToUpper(res.Verb), res.Affected, res.Duration)
	}
	line := fmt.Sprintf("%s%s ok — %d row(s) in %s", prefix, strings.ToUpper(res.Verb), len(res.Rows), res.Duration)
	if res.More {
		line += " (more truncated)"
	}
	return line
}

// resultJSON is the rows as an indented JSON array of objects.
func resultJSON(res *ExecResult) string {
	out := make([]map[string]any, 0, len(res.Rows))
	for _, rr := range res.Rows {
		obj := make(map[string]any, len(res.Columns))
		for i, col := range res.Columns {
			if i < len(rr) {
				obj[col] = jsonCell(rr[i])
			}
		}
		out = append(out, obj)
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// bytesText is the one display rule for wire bytes: bytes that read as text
// are text (MySQL returns every string column as bytes), 16 bytes are a UUID
// (Postgres's), anything else is hex.
func bytesText(b []byte) string {
	if isPrintableText(b) {
		return string(b)
	}
	if len(b) == 16 {
		return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	}
	return fmt.Sprintf("0x%x", b)
}

// isPrintableText reports whether b is UTF-8 with no control characters but
// tab, newline and carriage return.
func isPrintableText(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, r := range string(b) {
		if r != '\t' && r != '\n' && r != '\r' && unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// renderCell is one wire value as a single-line table cell.
func renderCell(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return renderCell(bytesText(x))
	case string:
		return strings.ReplaceAll(strings.ReplaceAll(x, "\n", "␤"), "\r", "")
	default:
		return fmt.Sprintf("%v", x)
	}
}

// jsonCell is one wire value for JSON, bytes by the same display rule.
func jsonCell(v any) any {
	if b, ok := v.([]byte); ok {
		return bytesText(b)
	}
	return v
}
