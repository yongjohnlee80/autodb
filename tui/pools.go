package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// THE CONNECTION POOLS MANAGER — pool.manage (admin): each pool's live budget,
// and creating, resizing and deleting pools. A pool is the socket budget the
// connections in it share; admins match pools to database servers. Moving a
// connection between pools is on the connection form, where the connection is.
//
// EACH SHARE READS held/limit, so a full share is visible at a glance: the
// front door's (sessions like a client's) and the headroom (the TUI, exec and
// autodb's own queries). WAITING is both shares' lines.

func newPoolsManager() *manager[PoolInfo] {
	return newManager("App.poolsStatus",
		func(ctx context.Context, b *Bound) ([]PoolInfo, error) { return b.Pools(ctx) },
		poolRow, "key", "name", "max", "frontDoor", "headroom", "waiting", "connections")
}

func poolRow(p PoolInfo) tuidecl.Row {
	return tuidecl.Row{
		"key": strconv.FormatInt(p.ID, 10), "name": p.Name,
		"max":         strconv.Itoa(p.MaxConns),
		"frontDoor":   fmt.Sprintf("%d/%d", p.FrontDoor, p.FrontDoorShare),
		"headroom":    fmt.Sprintf("%d/%d", p.Headroom, p.HeadroomShare),
		"waiting":     strconv.Itoa(p.FrontDoorWaiting + p.HeadroomWaiting),
		"connections": strconv.Itoa(len(p.Connections)),
	}
}

func poolManagerState(h *Host) map[string]any {
	return map[string]any{
		"App.poolRows": h.pools.model, "App.poolsStatus": "",
		"App.poolEditing": false, "App.poolFormNew": false, "App.poolIndex": 0,
		"App.poolChoices": h.poolChoices,
	}
}

// openPools is pool.manage.
func (h *Host) openPools() {
	if !h.session.IsAdmin() {
		return
	}
	h.poolEditID = 0
	h.set("App.poolEditing", false)
	h.set("App.poolIndex", 0)
	openManager(h, h.pools)
	h.open("pools")
}

func (h *Host) poolsClosed() error {
	h.pools.bound = nil
	h.set("App.poolEditing", false)
	return nil
}

func (h *Host) poolWritable() bool {
	b := h.pools.bound
	return b != nil && b.Gen() == h.session.Gen() && b.IdentityEpoch() == h.session.IdentityEpoch() &&
		b.User().Role == "admin"
}

// poolAdd is App.poolAdd: the form, for a new pool's name and max.
func (h *Host) poolAdd() error {
	if !h.poolWritable() {
		return nil
	}
	h.poolEditID = 0
	h.set("App.poolFormNew", true)
	h.set("App.poolEditing", true)
	h.set(h.pools.status, fmt.Sprintf("a new pool: its name, and the sockets autodb may hold to its "+
		"databases together (at least %d)", meta.MinPoolConns))
	return nil
}

// poolEdit is App.poolEdit(row): the form, for that pool's new max.
func (h *Host) poolEdit(i int) error {
	if !h.poolWritable() {
		return nil
	}
	p, ok := h.pools.at(i)
	if !ok {
		h.set(h.pools.status, "choose a pool first")
		return nil
	}
	h.poolEditID = p.ID
	h.set("App.poolFormNew", false)
	h.set("App.poolEditing", true)
	h.set(h.pools.status, fmt.Sprintf("%s: a new max (now %d). Lowering it closes nothing and drains as "+
		"work finishes; raising it serves waiters at once", p.Name, p.MaxConns))
	return nil
}

// poolSave is App.poolSave(name, max): creates a pool, or sets the edited
// pool's max. The server validates the range; this only parses the number.
func (h *Host) poolSave(name, maxText string) error {
	if !h.poolWritable() {
		return nil
	}
	max, err := strconv.Atoi(strings.TrimSpace(maxText))
	if err != nil {
		h.set(h.pools.status, "the max is a whole number of sockets")
		return nil
	}
	if id := h.poolEditID; id != 0 {
		managerCall(h, h.pools, "set max "+strconv.Itoa(max), func(ctx context.Context, b *Bound) error {
			return b.UpdatePool(ctx, id, max)
		})
	} else {
		name = strings.TrimSpace(name)
		if name == "" {
			h.set(h.pools.status, "a pool needs a name")
			return nil
		}
		managerCall(h, h.pools, "create pool "+name, func(ctx context.Context, b *Bound) error {
			_, err := b.CreatePool(ctx, name, max)
			return err
		})
	}
	h.set("App.poolEditing", false)
	return nil
}

// poolDelete is App.poolDelete(row): asked first.
func (h *Host) poolDelete(i int) error {
	if !h.poolWritable() {
		return nil
	}
	p, ok := h.pools.at(i)
	if !ok {
		h.set(h.pools.status, "choose a pool first")
		return nil
	}
	h.confirm("delete pool", "Delete pool "+p.Name+"? Only a pool no connection belongs to, archived "+
		"ones included, can be deleted, and the default pool never can.",
		"&Delete", "&Keep", func() {
			managerCall(h, h.pools, "delete pool "+p.Name, func(ctx context.Context, b *Bound) error {
				return b.DeletePool(ctx, p.ID)
			})
		})
	return nil
}

// loadPoolChoices fills the connection form's pool picker. Loaded with the
// connections manager, so the form opens on choices it already has.
func (h *Host) loadPoolChoices() {
	b := h.session.Bind()
	do(h, func(ctx context.Context) []PoolInfo {
		pools, err := b.Pools(ctx)
		if err != nil {
			return nil
		}
		return pools
	}, func(pools []PoolInfo) {
		h.poolList = pools
		rows := make([]tuidecl.Row, len(pools))
		for i, p := range pools {
			id := strconv.FormatInt(p.ID, 10)
			rows[i] = tuidecl.Row{"key": id, "id": id, "label": fmt.Sprintf("%s (%d)", p.Name, p.MaxConns)}
		}
		h.poolChoices.Reset(rows)
	})
}

// poolIndexOf is the picker index of a pool id, -1 when it isn't listed.
func (h *Host) poolIndexOf(id int64) int {
	for i, p := range h.poolList {
		if p.ID == id {
			return i
		}
	}
	return -1
}
