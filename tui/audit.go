package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
)

// The audit log, for an admin: everything the server recorded, searched by the
// server the way history is — f filters (connection, workspace, user, actions,
// dates), x clears, n / p page by the server's cursor.
//
// The audit log began recording connections with schema script 000003; rows
// before it name none, so a connection or workspace filter cannot match them.
// When such a filter is on, the help line says from when it covers.

func newAuditManager() *manager[AuditRow] {
	return newManager("App.auditStatus",
		func(context.Context, *Bound) ([]AuditRow, error) { return nil, nil }, // set per reload
		func(r AuditRow) tuidecl.Row {
			return tuidecl.Row{"when": r.CreatedAt, "who": r.User, "action": r.Action,
				"conn": r.Conn, "detail": strings.ReplaceAll(r.Detail, "\n", "␤")}
		}, "when", "who", "action", "conn", "detail")
}

const auditHelp = "f filter · x clear · n/p page"

// auditFilterIndexes are the audit filter form's choice positions.
var auditFilterIndexes = []string{"App.auditFilterConnIndex", "App.auditFilterSpaceIndex", "App.auditFilterUserIndex"}

// auditNav is the audit listing's filter, pages and form choices.
type auditNav struct {
	query                AuditQuery
	summary              string
	pages                []*AuditCursor
	next                 *AuditCursor
	since                int64 // the page's ConnFilterSince
	conns, spaces, users *tuidecl.ListModel
	formSeq              uint64
	pageSize             int64
}

func newAuditNav() *auditNav {
	return &auditNav{pages: []*AuditCursor{nil}, pageSize: historyPageSize,
		conns: tuidecl.NewListModel("key", "id", "label"), spaces: tuidecl.NewListModel("key", "id", "label"),
		users: tuidecl.NewListModel("key", "id", "label")}
}

func (h *Host) openAudit() {
	if !h.session.IsAdmin() {
		return
	}
	h.audit.rows, h.audit.all = nil, nil
	h.audit.model.Reset(nil)
	h.audit.bound = h.session.Bind()
	h.auditNav.query, h.auditNav.summary = AuditQuery{}, ""
	h.auditNav.pages, h.auditNav.next = []*AuditCursor{nil}, nil
	h.auditReload()
	h.open("audit")
}

func (h *Host) auditClosed() error { h.audit.bound = nil; h.auditNav.formSeq++; return nil }

// auditReload loads the current page, copying the query and cursor into the
// worker's load and bringing the next cursor back through the page it fills
// (see historyReload).
func (h *Host) auditReload() {
	q := h.auditNav.query
	q.Limit = h.auditNav.pageSize
	q.Before = h.auditNav.pages[len(h.auditNav.pages)-1]
	got := &AuditPage{}
	h.audit.load = func(ctx context.Context, b *Bound) ([]AuditRow, error) {
		p, err := b.SearchAudit(ctx, q)
		*got = p
		return p.Rows, err
	}
	h.audit.after = func() {
		h.auditNav.next, h.auditNav.since = got.Next, got.ConnFilterSince
		// The coverage note needs the page's answer, which arrives only now,
		// so it is set here; reloadManager then shows the line computed
		// before the load, and this corrects it.
		note := h.auditSinceNote()
		h.set("App.auditSince", note)
		h.set("App.auditSinceShown", note != "")
	}
	h.auditNav.next = nil
	reloadManager(h, h.audit, h.auditLine())
}

// auditSinceNote is the connection filter's coverage, when one is on.
func (h *Host) auditSinceNote() string {
	q := h.auditNav.query
	if (q.ConnID == 0 && q.WorkspaceID == 0) || h.auditNav.since == 0 {
		return ""
	}
	return "rows before " + time.Unix(h.auditNav.since, 0).Format("2006-01-02 15:04") +
		" name no connection, so this filter cannot match them"
}

func (h *Host) auditLine() string {
	line := auditHelp
	if n := len(h.auditNav.pages); n > 1 {
		line += fmt.Sprintf(" · page %d", n)
	}
	if h.auditNav.summary != "" {
		line += " · " + h.auditNav.summary
	}
	return line
}

func (h *Host) auditNextPage() error {
	if h.audit.bound == nil {
		return nil
	}
	if h.auditNav.next == nil {
		h.set(h.audit.status, "this is the last page · "+h.auditLine())
		return nil
	}
	h.auditNav.pages = append(h.auditNav.pages, h.auditNav.next)
	h.auditReload()
	return nil
}

func (h *Host) auditPrevPage() error {
	if h.audit.bound == nil {
		return nil
	}
	if len(h.auditNav.pages) <= 1 {
		h.set(h.audit.status, "this is the first page · "+h.auditLine())
		return nil
	}
	h.auditNav.pages = h.auditNav.pages[:len(h.auditNav.pages)-1]
	h.auditReload()
	return nil
}

func (h *Host) auditClearFilter() error {
	if h.audit.bound == nil {
		return nil
	}
	h.auditNav.query, h.auditNav.summary = AuditQuery{}, ""
	h.auditNav.pages = []*AuditCursor{nil}
	h.set("App.auditSince", "")
	h.set("App.auditSinceShown", false)
	h.auditReload()
	return nil
}

// auditFilterOpen fills the form's choices and opens it.
func (h *Host) auditFilterOpen() error {
	b := h.audit.bound
	if b == nil {
		return nil
	}
	h.auditNav.formSeq++
	seq := h.auditNav.formSeq
	type choices struct {
		conns  []ConnInfo
		spaces []WorkspaceInfo
		users  []UserRow
		err    error
	}
	h.set(h.audit.status, "loading the filter's choices…")
	do(h, func(ctx context.Context) choices {
		var c choices
		if c.conns, c.err = b.Connections(ctx); c.err != nil {
			return c
		}
		if c.spaces, c.err = b.Workspaces(ctx); c.err != nil {
			return c
		}
		c.users, c.err = b.Users(ctx)
		return c
	}, func(c choices) {
		if b != h.audit.bound || seq != h.auditNav.formSeq {
			return
		}
		if c.err != nil {
			h.set(h.audit.status, "filter: "+WireErrorMessage(c.err))
			return
		}
		fillChoices(h.auditNav.conns, "any connection", len(c.conns), func(i int) (int64, string) { return c.conns[i].ID, c.conns[i].Name })
		fillChoices(h.auditNav.spaces, "any workspace", len(c.spaces), func(i int) (int64, string) { return c.spaces[i].ID, c.spaces[i].Name })
		fillChoices(h.auditNav.users, "any user", len(c.users), func(i int) (int64, string) { return c.users[i].ID, c.users[i].Name })
		for _, k := range auditFilterIndexes {
			h.set(k, 0)
		}
		h.set(h.audit.status, h.auditLine())
		h.open("auditFilter")
	})
	return nil
}

// fillChoices resets m to "any" and then the n items id/label gives.
func fillChoices(m *tuidecl.ListModel, any string, n int, item func(i int) (int64, string)) {
	rows := []tuidecl.Row{{"key": "0", "id": "0", "label": any}}
	for i := 0; i < n; i++ {
		id, label := item(i)
		v := strconv.FormatInt(id, 10)
		rows = append(rows, tuidecl.Row{"key": v, "id": v, "label": label})
	}
	m.Reset(rows)
}

func (h *Host) auditFilterCancelled() error { h.auditNav.formSeq++; return nil }

// auditFilterApply sets the filter from the form: actions are a comma-separated
// list, the dates days in local time with To inclusive.
func (h *Host) auditFilterApply(conn, space, user, actions, from, to string) error {
	if h.audit.bound == nil {
		return nil
	}
	var q AuditQuery
	var words []string
	fail := func(err error) error { h.set(h.audit.status, "filter: "+err.Error()); return nil }
	var err error
	if q.ConnID, err = choiceID(conn, h.auditNav.conns, "connection", &words); err != nil {
		return fail(err)
	}
	if q.WorkspaceID, err = choiceID(space, h.auditNav.spaces, "workspace", &words); err != nil {
		return fail(err)
	}
	if q.UserID, err = choiceID(user, h.auditNav.users, "user", &words); err != nil {
		return fail(err)
	}
	for _, a := range strings.Split(actions, ",") {
		if a = strings.TrimSpace(a); a != "" {
			q.Actions = append(q.Actions, a)
		}
	}
	if len(q.Actions) > 0 {
		words = append(words, "action "+strings.Join(q.Actions, "|"))
	}
	f, t, err := dayRange(from, to)
	if err != nil {
		return fail(err)
	}
	if !f.IsZero() {
		q.From = f.Unix()
		words = append(words, "from "+from)
	}
	if !t.IsZero() {
		q.To = t.AddDate(0, 0, 1).Unix()
		words = append(words, "to "+to)
	}
	h.auditNav.query = q
	h.auditNav.summary = strings.Join(words, ", ")
	h.auditNav.pages = []*AuditCursor{nil}
	h.auditReload()
	return nil
}

// choiceID reads a form's choice id and names it in words; "" and "0" are "any".
func choiceID(v string, m *tuidecl.ListModel, what string, words *[]string) (int64, error) {
	if v == "" || v == "0" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not an id", what, v)
	}
	for i := 0; i < m.Len(); i++ {
		if m.At(i)["id"] == v {
			*words = append(*words, fmt.Sprintf("%s %v", what, m.At(i)["label"]))
		}
	}
	return n, nil
}

// dayRange parses a form's from/to days (YYYY-MM-DD, local time); either may
// be empty, and to may not be before from.
func dayRange(from, to string) (time.Time, time.Time, error) {
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
	f, err := day(from, "from")
	if err != nil {
		return f, f, err
	}
	t, err := day(to, "to")
	if err != nil {
		return f, t, err
	}
	if !f.IsZero() && !t.IsZero() && t.Before(f) {
		return f, t, fmt.Errorf("to %s is before from %s", to, from)
	}
	return f, t, nil
}
