package tui

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"

	"github.com/yongjohnlee80/autodb/core/engine"
)

// THE CONNECTIONS MANAGER — conn.manage: add, edit, test, delete, and attach
// to a workspace.
//
// A row shows the two decisions an operator most often confuses, side by
// side: PROXY — whether the front door carries it — and PROFILE — what SQL a
// client may send over it. They are independent, and a connection that
// authenticates and then refuses its client's opening statement looked, with
// only one of them shown, exactly like a working one.
//
// EDITING SENDS ONLY WHAT CHANGED, each through its own audited call: a
// rename, the exposure and the profile stay three separate changes in the
// record, as they are three decisions. Opening the front door keeps its
// consent step — it puts a database on the network — asked after the form has
// closed. Deleting asks first: it is not undoable, and takes grants and
// workspace links with it.

// The profile vocabulary, as literals: an exposure surface carries profile
// values as opaque strings and never names core/exec's constants.
var connProfiles = []tuidecl.Row{
	{"key": "v1compat", "id": "v1compat", "label": "v1compat — refuses SET/BEGIN; breaks most SQL clients"},
	{"key": "session", "id": "session", "label": "session — admits session state over the front door"},
}

var proxyChoices = []tuidecl.Row{
	{"key": "yes", "id": "yes", "label": "yes"},
	{"key": "no", "id": "no", "label": "no"},
}

// connForm is what the connection form is for: adding (edit nil) or editing.
type connForm struct {
	edit *ConnInfo
}

// attachFor is the connection the attach dialog is choosing a workspace for.
type attachFor struct {
	id   int64
	name string
}

func newConnectionsManager() *manager[ConnInfo] {
	return newManager("App.connectionsStatus",
		func(ctx context.Context, b *Bound) ([]ConnInfo, error) { return b.Connections(ctx) },
		func(c ConnInfo) tuidecl.Row {
			return tuidecl.Row{
				"key": strconv.FormatInt(c.ID, 10), "id": strconv.FormatInt(c.ID, 10), "name": c.Name,
				"engine": c.Engine, "profile": profileOrDefault(c.Profile), "proxy": yesNo(c.FrontDoorExposed),
				"target": c.TargetDB,
			}
		}, "key", "id", "name", "engine", "profile", "proxy", "target")
}

// connectionsState is the manager's and its forms' sources.
func connectionsState(h *Host) map[string]any {
	engines := tuidecl.NewListModel("key", "id", "label")
	var rows []tuidecl.Row
	for _, n := range engine.All() {
		rows = append(rows, tuidecl.Row{"key": string(n), "id": string(n), "label": string(n)})
	}
	engines.Reset(rows)
	profiles := tuidecl.NewListModel("key", "id", "label")
	profiles.Reset(connProfiles)
	proxy := tuidecl.NewListModel("key", "id", "label")
	proxy.Reset(proxyChoices)
	return map[string]any{
		"App.connectionRows":    h.conns.model,
		"App.connectionsStatus": "",
		"App.engines":           engines,
		"App.proxyChoices":      proxy,
		"App.profileChoices":    profiles,
		"App.connFormTitle":     "",
		"App.connFormError":     "",
		"App.connFormName":      "",
		"App.connFormAdding":    true,
		"App.connFormEditing":   false,
		"App.connFormEngine":    -1,
		"App.connFormProxy":     -1,
		"App.connFormProfile":   -1,
		"App.attachTitle":       "",
		"App.attachError":       "",
		"App.attachWorkspaces":  h.attachWs,
		"App.attachWorkspace":   -1,
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// profileOrDefault is a row's profile, or the column default for one that
// names none.
func profileOrDefault(p string) string {
	if p == "" {
		return "v1compat"
	}
	return p
}

// openConnections is conn.manage.
func (h *Host) openConnections() {
	openManager(h, h.conns)
	h.open("connections")
}

// connectionAdd is App.connectionAdd: the form, adding.
func (h *Host) connectionAdd() error {
	h.connForm = connForm{}
	h.showConnForm("new connection", "", -1, -1, -1)
	return nil
}

// connectionEdit is App.connectionEdit(row): the form, editing that row.
func (h *Host) connectionEdit(i int) error {
	c, ok := h.conns.at(i)
	if !ok {
		h.set(h.conns.status, "choose a connection first")
		return nil
	}
	h.connForm = connForm{edit: &c}
	proxy := 1
	if c.FrontDoorExposed {
		proxy = 0
	}
	profile := slices.IndexFunc(connProfiles, func(r tuidecl.Row) bool { return r["id"] == profileOrDefault(c.Profile) })
	h.showConnForm("edit "+c.Name, c.Name, -1, proxy, profile)
	return nil
}

// showConnForm opens the form with its fields as given.
func (h *Host) showConnForm(title, name string, engine, proxy, profile int) {
	adding := h.connForm.edit == nil
	state := map[string]any{
		"App.connFormTitle": title, "App.connFormError": "",
		"App.connFormAdding": adding, "App.connFormEditing": !adding,
	}
	// Through the empty value: a binding applies what changes, and the form
	// must open on these even when it last opened on the same.
	for k, v := range map[string]any{"App.connFormName": "", "App.connFormEngine": -1, "App.connFormProxy": -1, "App.connFormProfile": -1} {
		state[k] = v
	}
	if err := h.p.SetMany(state); err != nil {
		h.keep(err)
		return
	}
	if err := h.p.SetMany(map[string]any{
		"App.connFormName": name, "App.connFormEngine": engine, "App.connFormProxy": proxy, "App.connFormProfile": profile,
	}); err != nil {
		h.keep(err)
		return
	}
	h.open("connectionForm")
}

// refuseConnForm opens the form again, saying why.
func (h *Host) refuseConnForm(why string) error {
	h.set("App.connFormError", why)
	h.p.Post(func() { h.open("connectionForm") })
	return nil
}

// saveConnection is App.saveConnection(name, engine, dsn, proxy, profile).
func (h *Host) saveConnection(name, eng, dsn, proxy, profile string) error {
	m := h.conns
	if h.connForm.edit == nil {
		if name == "" || eng == "" || dsn == "" {
			return h.refuseConnForm("a name, an engine and a DSN are all required")
		}
		managerCall(h, m, "create "+name, func(ctx context.Context, b *Bound) error {
			_, err := b.CreateConnection(ctx, name, eng, dsn)
			return err
		})
		return nil
	}
	sel := *h.connForm.edit
	if name == "" {
		return h.refuseConnForm("a name is required")
	}
	apply := func() {
		managerCall(h, m, "edit "+sel.Name, func(ctx context.Context, b *Bound) error {
			if name != sel.Name {
				if err := b.RenameConnection(ctx, sel.ID, name); err != nil {
					return err
				}
			}
			if profile != "" && profile != profileOrDefault(sel.Profile) {
				if err := b.SetConnectionProfile(ctx, sel.ID, profile); err != nil {
					return err
				}
			}
			if proxy != "" && (proxy == "yes") != sel.FrontDoorExposed {
				return b.SetConnectionExposure(ctx, sel.ID, proxy == "yes")
			}
			return nil
		})
	}
	if proxy == "yes" && !sel.FrontDoorExposed {
		target := sel.TargetDB
		if target == "" {
			target = "(none recorded — clients use the connection name)"
		}
		// After the form has closed: its accepted handler is still running.
		h.p.Post(func() {
			h.confirm("open the front door on "+sel.Name+"?",
				frontDoorProse+"\nClients would connect with Database = "+target+"\nor the connection name "+sel.Name+".",
				"&Expose it", "&Leave it closed", apply)
		})
		return nil
	}
	apply()
	return nil
}

// frontDoorProse is what an operator reads BEFORE exposing a connection.
const frontDoorProse = `This makes the connection reachable through the front door. It does
not change what SQL clients may send — that is the capability profile,
set separately on this same form.

Reaching it still takes an access token bound to this connection, a
grant on it, and a source address the account is admitted from.

The change is recorded in the audit log.
`

// connectionTest is App.connectionTest(row).
func (h *Host) connectionTest(i int) error {
	c, ok := h.conns.at(i)
	if !ok {
		h.set(h.conns.status, "choose a connection first")
		return nil
	}
	managerCall(h, h.conns, "test "+c.Name, func(ctx context.Context, b *Bound) error { return b.TestConnection(ctx, c.ID) })
	return nil
}

// connectionDelete is App.connectionDelete(row): asked first.
func (h *Host) connectionDelete(i int) error {
	c, ok := h.conns.at(i)
	if !ok {
		h.set(h.conns.status, "choose a connection first")
		return nil
	}
	h.confirm("delete connection", "Delete "+c.Name+"? Its grants and workspace links go with it. This cannot be undone.",
		"&Delete", "&Keep", func() {
			managerCall(h, h.conns, "delete "+c.Name, func(ctx context.Context, b *Bound) error { return b.DeleteConnection(ctx, c.ID) })
		})
	return nil
}

// connectionAttach is App.connectionAttach(row): which workspace to add it to.
func (h *Host) connectionAttach(i int) error {
	c, ok := h.conns.at(i)
	if !ok {
		h.set(h.conns.status, "choose a connection first")
		return nil
	}
	h.attaching = attachFor{id: c.ID, name: c.Name}
	bound := h.conns.bound
	type listed struct {
		wss []WorkspaceInfo
		err error
	}
	do(h, func(ctx context.Context) listed {
		wss, err := bound.Workspaces(ctx)
		return listed{wss: wss, err: err}
	}, func(l listed) {
		if l.err != nil {
			h.set(h.conns.status, "workspaces: "+WireErrorMessage(l.err))
			return
		}
		var rows []tuidecl.Row
		for _, w := range l.wss {
			if slices.ContainsFunc(w.Connections, func(x ConnInfo) bool { return x.ID == c.ID }) {
				continue // already there
			}
			rows = append(rows, tuidecl.Row{"key": strconv.FormatInt(w.ID, 10), "id": w.ID, "name": w.Name})
		}
		if len(rows) == 0 {
			h.set(h.conns.status, c.Name+" is in every workspace already")
			return
		}
		h.attachWs.Reset(rows)
		h.set("App.attachTitle", "attach "+c.Name+" to a workspace")
		h.set("App.attachError", "")
		h.set("App.attachWorkspace", -1)
		h.set("App.attachWorkspace", 0)
		h.open("attach")
	})
	return nil
}

// attachConnection is App.attachConnection(workspace): the attach answered.
func (h *Host) attachConnection(wsID int64, _ string) error {
	a := h.attaching
	if wsID == 0 {
		h.set("App.attachError", "choose a workspace")
		h.p.Post(func() { h.open("attach") })
		return nil
	}
	managerCall(h, h.conns, fmt.Sprintf("attach %s to workspace %d", a.name, wsID), func(ctx context.Context, b *Bound) error {
		return b.AttachConnection(ctx, wsID, a.id)
	})
	return nil
}
