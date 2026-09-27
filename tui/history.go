package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
)

// Script history comes from the Bound RPC, never from client-side editor
// buffers. The compact QML table delegates all actions to the host.
//
// The listing is a SEARCH, filtered and paged by the server: the filter form
// (f) narrows it by connection, workspace, user, status and dates, and n / p
// move through its pages by the server's cursor. Nothing is filtered here —
// a page is the rows the server chose, so what the table shows is what the
// filter asked for.
func newHistoryManager() *manager[HistoryRow] {
	return newManager("App.historyStatus",
		func(context.Context, *Bound) ([]HistoryRow, error) { return nil, nil }, // set per reload
		func(r HistoryRow) tuidecl.Row {
			status := r.Status
			// The one case status cannot say: a refusal is recorded as
			// error, which is what every reader before dispositions saw, and
			// the disposition is what tells it from a failure. The rest of
			// the disposition is on the script card (Enter), where it costs
			// the SCRIPT column nothing.
			if r.Disposition == "refused" {
				status = "refused"
			}
			if r.Suspended {
				status += " (suspended)"
			}
			return tuidecl.Row{"when": r.StartedAt, "who": r.User, "conn": r.Conn,
				"status": status, "rows": r.RowCount, "took": r.Duration.String(),
				"script": strings.ReplaceAll(strings.ReplaceAll(r.Script, "\n", "␤"), "\r", "")}
		}, "when", "who", "conn", "status", "rows", "took", "script")
}

// historyHelp is the history dialog's key line.
const historyHelp = "Enter script · e load · y copy · f filter · x clear · n/p page"

// historyFilterIndexes are the filter form's choice positions, reset to "any"
// each time it opens.
var historyFilterIndexes = []string{"App.historyFilterConnIndex", "App.historyFilterSpaceIndex",
	"App.historyFilterUserIndex", "App.historyFilterStatusIndex"}

// historyPageSize is how many rows one page holds.
const historyPageSize = 100

// historyStatuses are the statuses the filter offers; the server validates.
var historyStatuses = []string{"ok", "ok_pending_commit", "error", "rolled_back", "outcome_unresolvable", "unknown", "running"}

// historyNav is the listing's filter, its pages, and the filter form's choices.
type historyNav struct {
	query                          HistoryQuery
	summary                        string           // the filter in words, for the help line
	pages                          []*HistoryCursor // the cursor each shown page started at; nil is the first
	next                           *HistoryCursor   // where the page after the current one starts; nil at the end
	conns, spaces, users, statuses *tuidecl.ListModel
	formSeq                        uint64
	pageSize                       int64 // historyPageSize; a test seam
}

func newHistoryNav() *historyNav {
	n := &historyNav{pages: []*HistoryCursor{nil}, pageSize: historyPageSize,
		conns: tuidecl.NewListModel("key", "id", "label"), spaces: tuidecl.NewListModel("key", "id", "label"),
		users: tuidecl.NewListModel("key", "id", "label"), statuses: tuidecl.NewListModel("key", "id", "label")}
	rows := []tuidecl.Row{{"key": "", "id": "", "label": "any status"}}
	for _, st := range historyStatuses {
		rows = append(rows, tuidecl.Row{"key": st, "id": st, "label": st})
	}
	n.statuses.Reset(rows)
	return n
}

func (h *Host) openHistory() {
	h.history.rows, h.history.all = nil, nil
	h.history.model.Reset(nil)
	h.history.bound = h.session.Bind()
	h.hist.query, h.hist.summary = HistoryQuery{}, ""
	h.hist.pages, h.hist.next = []*HistoryCursor{nil}, nil
	h.historyReload()
	h.open("history")
}

// historyReload loads the current page of the current filter.
//
// The query and the cursor are COPIED into the load, which a worker runs: the
// loop owns them and may change them before it starts. The next page's cursor
// comes back the same way, through the page the load fills, read on the loop
// once the rows are in.
func (h *Host) historyReload() {
	q := h.hist.query
	q.Limit = h.hist.pageSize
	q.Before = h.hist.pages[len(h.hist.pages)-1]
	page := &struct{ next *HistoryCursor }{}
	h.history.load = func(ctx context.Context, b *Bound) ([]HistoryRow, error) {
		rows, next, err := b.SearchHistory(ctx, q)
		page.next = next
		return rows, err
	}
	h.history.after = func() { h.hist.next = page.next }
	h.hist.next = nil
	reloadManager(h, h.history, h.historyLine())
}

// historyLine is the help line: the keys, the page, and the filter in words.
func (h *Host) historyLine() string {
	line := historyHelp
	if n := len(h.hist.pages); n > 1 {
		line += fmt.Sprintf(" · page %d", n)
	}
	if h.hist.summary != "" {
		line += " · " + h.hist.summary
	}
	return line
}

func (h *Host) historyNextPage() error {
	if h.history.bound == nil {
		return nil
	}
	if h.hist.next == nil {
		h.set(h.history.status, "this is the last page · "+h.historyLine())
		return nil
	}
	h.hist.pages = append(h.hist.pages, h.hist.next)
	h.historyReload()
	return nil
}

func (h *Host) historyPrevPage() error {
	if h.history.bound == nil {
		return nil
	}
	if len(h.hist.pages) <= 1 {
		h.set(h.history.status, "this is the first page · "+h.historyLine())
		return nil
	}
	h.hist.pages = h.hist.pages[:len(h.hist.pages)-1]
	h.historyReload()
	return nil
}

func (h *Host) historyClearFilter() error {
	if h.history.bound == nil {
		return nil
	}
	h.hist.query, h.hist.summary = HistoryQuery{}, ""
	h.hist.pages = []*HistoryCursor{nil}
	h.historyReload()
	return nil
}

// historyFilterOpen fills the form's choices — the connections, workspaces
// and, for an admin, the users the caller can see — and opens it.
func (h *Host) historyFilterOpen() error {
	b := h.history.bound
	if b == nil {
		return nil
	}
	h.hist.formSeq++
	seq := h.hist.formSeq
	admin := h.session.IsAdmin()
	type choices struct {
		conns  []ConnInfo
		spaces []WorkspaceInfo
		users  []UserRow
		err    error
	}
	h.set(h.history.status, "loading the filter's choices…")
	do(h, func(ctx context.Context) choices {
		var c choices
		if c.conns, c.err = b.Connections(ctx); c.err != nil {
			return c
		}
		if c.spaces, c.err = b.Workspaces(ctx); c.err != nil {
			return c
		}
		if admin {
			c.users, c.err = b.Users(ctx)
		}
		return c
	}, func(c choices) {
		if b != h.history.bound || seq != h.hist.formSeq {
			return
		}
		if c.err != nil {
			h.set(h.history.status, "filter: "+WireErrorMessage(c.err))
			return
		}
		fill := func(m *tuidecl.ListModel, all string, items func(add func(id int64, label string))) {
			rows := []tuidecl.Row{{"key": "0", "id": "0", "label": all}}
			items(func(id int64, label string) {
				v := strconv.FormatInt(id, 10)
				rows = append(rows, tuidecl.Row{"key": v, "id": v, "label": label})
			})
			m.Reset(rows)
		}
		fill(h.hist.conns, "any connection", func(add func(int64, string)) {
			for _, x := range c.conns {
				add(x.ID, x.Name)
			}
		})
		fill(h.hist.spaces, "any workspace", func(add func(int64, string)) {
			for _, x := range c.spaces {
				add(x.ID, x.Name)
			}
		})
		fill(h.hist.users, "any user", func(add func(int64, string)) {
			for _, x := range c.users {
				add(x.ID, x.Name)
			}
		})
		h.set("App.historyFilterUsers", admin)
		for _, k := range historyFilterIndexes {
			h.set(k, 0) // every field starts at "any"
		}
		h.set(h.history.status, h.historyLine())
		h.open("historyFilter")
	})
	return nil
}

func (h *Host) historyFilterCancelled() error { h.hist.formSeq++; return nil }

// historyFilterApply sets the filter from the form and lists its first page.
// The dates are days, in local time: From from the start of its day, To
// through the end of its own — the form says "to (inclusive)".
func (h *Host) historyFilterApply(conn, space, user, status, from, to string) error {
	if h.history.bound == nil {
		return nil
	}
	var q HistoryQuery
	var words []string
	num := func(v string, m *tuidecl.ListModel, what string) (int64, error) {
		if v == "" || v == "0" {
			return 0, nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s %q is not an id", what, v)
		}
		for i := 0; i < m.Len(); i++ {
			if m.At(i)["id"] == v {
				words = append(words, fmt.Sprintf("%s %v", what, m.At(i)["label"]))
			}
		}
		return n, nil
	}
	day := func(v, what string) (time.Time, error) {
		if v = strings.TrimSpace(v); v == "" {
			return time.Time{}, nil
		}
		d, err := time.ParseInLocation("2006-01-02", v, time.Local)
		if err != nil {
			return time.Time{}, fmt.Errorf("%s %q is not a date (YYYY-MM-DD)", what, v)
		}
		return d, nil
	}
	var err error
	fail := func(err error) error { h.set(h.history.status, "filter: "+err.Error()); return nil }
	if q.ConnID, err = num(conn, h.hist.conns, "connection"); err != nil {
		return fail(err)
	}
	if q.WorkspaceID, err = num(space, h.hist.spaces, "workspace"); err != nil {
		return fail(err)
	}
	if q.UserID, err = num(user, h.hist.users, "user"); err != nil {
		return fail(err)
	}
	if status != "" {
		q.Status = status
		words = append(words, "status "+status)
	}
	f, err := day(from, "from")
	if err != nil {
		return fail(err)
	}
	t, err := day(to, "to")
	if err != nil {
		return fail(err)
	}
	if !f.IsZero() && !t.IsZero() && t.Before(f) {
		return fail(fmt.Errorf("to %s is before from %s", to, from))
	}
	if !f.IsZero() {
		q.From = f.Unix()
		words = append(words, "from "+from)
	}
	if !t.IsZero() {
		q.To = t.AddDate(0, 0, 1).Unix() // through the end of the To day
		words = append(words, "to "+to)
	}
	h.hist.query = q
	h.hist.summary = strings.Join(words, ", ")
	h.hist.pages = []*HistoryCursor{nil}
	h.historyReload()
	return nil
}

func (h *Host) historyClosed() error { h.history.bound = nil; return nil }

func (h *Host) historyRow(i int) (HistoryRow, bool) {
	r, ok := h.history.at(i)
	if !ok {
		h.set(h.history.status, "choose a history row first")
	}
	return r, ok
}

func (h *Host) historyShow(i int) error {
	r, ok := h.historyRow(i)
	if !ok {
		return nil
	}
	h.valueText = r.Script
	title := fmt.Sprintf("script — %s", r.StartedAt)
	if r.Disposition != "" {
		title += " · " + r.Disposition // what the attempt did, beside its status
	}
	h.set("App.valueTitle", title)
	h.set("App.valueText", r.Script)
	// By the connection's id, never its name: a name is a label, and one
	// the explorer does not list (another workspace's) reads as the default.
	h.set("App.valueSyntax", syntaxFor(h.explorer.engines[r.ConnID]))
	h.open("value")
	return nil
}

func (h *Host) historyLoad(i int) error {
	r, ok := h.historyRow(i)
	if !ok {
		return nil
	}
	if err := h.p.Call("history", "close"); err != nil {
		return err
	}
	h.history.bound = nil
	h.scaffold(r.Script) // asks about an unsaved note before replacing its buffer
	return nil
}

func (h *Host) historyCopy(i int) error {
	r, ok := h.historyRow(i)
	if !ok {
		return nil
	}
	h.editor.SetRegister(r.Script, false)
	if h.p.App().CopyToClipboard(r.Script) {
		h.set(h.history.status, "script copied to clipboard and editor register")
	} else {
		h.set(h.history.status, "clipboard unavailable — script copied to editor register")
	}
	return nil
}
