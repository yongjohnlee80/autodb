package tui

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
)

// Users are admin-only. The host owns actions and server-bound identity;
// QML presents one table and one mode-dependent form.
func newUserManager() *manager[UserRow] {
	return newManager("App.usersStatus",
		func(ctx context.Context, b *Bound) ([]UserRow, error) { return b.Users(ctx) },
		func(u UserRow) tuidecl.Row {
			state := "active"
			if u.Disabled {
				state = "disabled"
			}
			return tuidecl.Row{"key": strconv.FormatInt(u.ID, 10), "id": u.ID,
				"name": u.Name, "role": u.Role, "state": state}
		}, "key", "id", "name", "role", "state")
}

var userRoleNames = []string{"reader", "editor", "admin"}

func userRoleModel() *tuidecl.ListModel {
	m := tuidecl.NewListModel("key", "id", "label")
	var rows []tuidecl.Row
	for _, role := range userRoleNames {
		rows = append(rows, tuidecl.Row{"key": role, "id": role, "label": role})
	}
	m.Reset(rows)
	return m
}

func userManagerState(h *Host) map[string]any {
	return map[string]any{
		"App.userRows": h.users.model, "App.usersStatus": "", "App.userIndex": 0,
		"App.roles": h.userRoles, "App.userConnections": h.userConnections,
		"App.userFormTitle": "", "App.userFormError": "",
		"App.userFormNaming": false, "App.userFormRoling": false,
		"App.userFormGranting": false, "App.userFormPassphrase": false,
		"App.userFormRoleIndex": -1, "App.userFormConnIndex": -1,
	}
}

func (h *Host) openUsers() {
	if !h.session.IsAdmin() {
		return
	}
	h.users.rows, h.users.all = nil, nil
	h.users.model.Reset(nil)
	h.set("App.userIndex", 0)
	openManager(h, h.users)
	h.open("users")
}

func (h *Host) usersClosed() error {
	h.users.bound, h.userFormBound = nil, nil
	h.userFormSeq++
	h.userConnections.Reset(nil)
	return nil
}

func (h *Host) userFormCancelled() error {
	h.userFormBound = nil
	h.userFormSeq++
	return nil
}

func (h *Host) userWritable() bool {
	b := h.users.bound
	return b != nil && b.User().Role == "admin" && b.Gen() == h.session.Gen() &&
		b.IdentityEpoch() == h.session.IdentityEpoch()
}

func (h *Host) selectedUser(i int) (UserRow, bool) {
	u, ok := h.users.at(i)
	if !ok {
		h.set(h.users.status, "choose a user first")
	}
	return u, ok
}

func (h *Host) showUserForm(mode string, userID int64, title string, roleIndex, connIndex int) {
	h.userFormSeq++
	h.userFormBound, h.userFormMode, h.userFormUserID = h.users.bound, mode, userID
	if err := h.p.SetMany(map[string]any{
		"App.userFormTitle": title, "App.userFormError": "",
		"App.userFormNaming": mode == "new", "App.userFormRoling": mode == "new" || mode == "role" || mode == "grant",
		"App.userFormGranting": mode == "grant", "App.userFormPassphrase": mode == "new" || mode == "reset",
		"App.userFormRoleIndex": -1, "App.userFormConnIndex": -1,
	}); err != nil {
		h.keep(err)
		return
	}
	h.set("App.userFormRoleIndex", roleIndex)
	h.set("App.userFormConnIndex", connIndex)
	h.open("userForm")
}

func (h *Host) userAdd() error {
	if h.userWritable() {
		h.showUserForm("new", 0, "new user", -1, -1)
	}
	return nil
}

func (h *Host) userRole(i int) error {
	if !h.userWritable() {
		return nil
	}
	u, ok := h.selectedUser(i)
	if ok {
		h.showUserForm("role", u.ID, "role for "+u.Name, slices.Index(userRoleNames, u.Role), -1)
	}
	return nil
}

func (h *Host) userResetPassphrase(i int) error {
	if !h.userWritable() {
		return nil
	}
	u, ok := h.selectedUser(i)
	if ok {
		h.showUserForm("reset", u.ID, "reset passphrase for "+u.Name, -1, -1)
	}
	return nil
}

func (h *Host) userToggle(i int) error {
	if !h.userWritable() {
		return nil
	}
	u, ok := h.selectedUser(i)
	if ok {
		managerCall(h, h.users, "toggle "+u.Name, func(ctx context.Context, b *Bound) error {
			return b.SetUserDisabled(ctx, u.ID, !u.Disabled)
		})
	}
	return nil
}

func (h *Host) userRemove(i int) error {
	if !h.userWritable() {
		return nil
	}
	u, ok := h.selectedUser(i)
	if !ok {
		return nil
	}
	bound := h.users.bound
	h.confirm("remove user", "Remove "+u.Name+"? Its grants and tokens are removed. This cannot be undone.",
		"&Remove", "&Keep", func() {
			if bound != h.users.bound || !h.userWritable() {
				return
			}
			managerCall(h, h.users, "remove "+u.Name, func(ctx context.Context, b *Bound) error {
				return b.RemoveUser(ctx, u.ID)
			})
		})
	return nil
}

func (h *Host) userGrant(i int) error {
	if !h.userWritable() {
		return nil
	}
	u, ok := h.selectedUser(i)
	if !ok {
		return nil
	}
	b := h.users.bound
	h.userFormSeq++
	seq := h.userFormSeq
	h.set(h.users.status, "loading connections for grant…")
	type listed struct {
		conns []ConnInfo
		err   error
	}
	do(h, func(ctx context.Context) listed {
		rows, err := b.Connections(ctx)
		return listed{rows, err}
	}, func(v listed) {
		if b != h.users.bound || !h.userWritable() || seq != h.userFormSeq {
			return
		}
		if v.err != nil {
			h.set(h.users.status, WireErrorMessage(v.err))
			return
		}
		var choices []tuidecl.Row
		for _, c := range v.conns {
			id := strconv.FormatInt(c.ID, 10)
			choices = append(choices, tuidecl.Row{"key": id, "id": id, "label": c.Name + "  (" + c.Engine + ")"})
		}
		if len(choices) == 0 {
			h.set(h.users.status, "no visible connections to grant")
			return
		}
		h.userConnections.Reset(choices)
		h.set(h.users.status, "")
		h.showUserForm("grant", u.ID, "grant for "+u.Name, -1, -1)
	})
	return nil
}

func (h *Host) userIPs(i int) error {
	if !h.userWritable() {
		return nil
	}
	u, ok := h.selectedUser(i)
	if ok {
		h.openUserAddresses(u.ID, u.Name)
	}
	return nil
}

func (h *Host) refuseUserForm(reason string) error {
	h.set("App.userFormError", reason)
	h.p.Post(func() { h.open("userForm") })
	return nil
}

func (h *Host) saveUser(name, role, connText, pass string) error {
	if !h.userWritable() || h.userFormBound != h.users.bound {
		return nil
	}
	mode, userID := h.userFormMode, h.userFormUserID
	if (mode == "new" || mode == "role" || mode == "grant") && !slices.Contains(userRoleNames, role) {
		return h.refuseUserForm("choose a role")
	}
	switch mode {
	case "new":
		name = strings.TrimSpace(name)
		if name == "" || len(pass) < 8 {
			return h.refuseUserForm("a name and passphrase of at least 8 characters are required")
		}
		managerCall(h, h.users, "create "+name, func(ctx context.Context, b *Bound) error {
			_, err := b.CreateUser(ctx, name, pass, role)
			return err
		})
	case "role":
		managerCall(h, h.users, "role", func(ctx context.Context, b *Bound) error { return b.SetUserRole(ctx, userID, role) })
	case "reset":
		if len(pass) < 8 {
			return h.refuseUserForm("passphrase must be at least 8 characters")
		}
		managerCall(h, h.users, "reset passphrase", func(ctx context.Context, b *Bound) error { return b.ResetUserPassphrase(ctx, userID, pass) })
	case "grant":
		connID, err := strconv.ParseInt(connText, 10, 64)
		if err != nil || connID <= 0 {
			return h.refuseUserForm("choose a connection")
		}
		valid := false
		for i := 0; i < h.userConnections.Len(); i++ {
			if h.userConnections.At(i)["id"] == connText {
				valid = true
				break
			}
		}
		if !valid {
			return h.refuseUserForm("connection is no longer offered")
		}
		managerCall(h, h.users, "grant", func(ctx context.Context, b *Bound) error { return b.AddGrant(ctx, userID, connID, role) })
	default:
		return fmt.Errorf("unknown user form mode %q", mode)
	}
	h.userFormBound = nil
	return nil
}
