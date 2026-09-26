package tui

import (
	"context"
	"net/netip"
	"strconv"
	"strings"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
)

// addressRow is the one UI shape for config-seeded global CIDRs, managed
// global CIDRs, and a caller's or selected user's personal allowlist.
type addressRow struct {
	id                 int64
	cidr, note, source string
	config             bool
}

func newAddressManager() *manager[addressRow] {
	return newManager("App.addressesStatus",
		func(context.Context, *Bound) ([]addressRow, error) { return nil, nil },
		func(r addressRow) tuidecl.Row {
			key := r.source + ":" + r.cidr
			if r.id != 0 {
				key = r.source + ":" + strconv.FormatInt(r.id, 10)
			}
			return tuidecl.Row{"key": key, "source": r.source, "cidr": r.cidr, "note": r.note}
		}, "key", "source", "cidr", "note")
}

func addressManagerState(h *Host) map[string]any {
	return map[string]any{
		"App.addressRows": h.addresses.model, "App.addressesTitle": "",
		"App.addressesStatus": "", "App.addressAdding": false, "App.addressIndex": 0,
	}
}

func (h *Host) openGlobalAddresses() {
	if !h.session.IsAdmin() {
		return
	}
	h.addressGlobal, h.addressUserID = true, 0
	h.addresses.load = func(ctx context.Context, b *Bound) ([]addressRow, error) {
		if h.addressTrace != nil {
			h.addressTrace("global")
		}
		entries, err := b.Allowlist(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]addressRow, len(entries))
		for i, e := range entries {
			source := "store"
			if e.Config {
				source = "config"
			}
			out[i] = addressRow{id: e.ID, cidr: e.CIDR, note: e.Note, source: source, config: e.Config}
		}
		return out, nil
	}
	h.openAddressManager("ip allowlist (global)")
}

func (h *Host) openUserAddresses(userID int64, who string) {
	if userID == 0 || h.session.Token() == "" {
		return
	}
	h.addressGlobal, h.addressUserID = false, userID
	h.addresses.load = func(ctx context.Context, b *Bound) ([]addressRow, error) {
		if h.addressTrace != nil {
			h.addressTrace("personal")
		}
		entries, err := b.UserIPs(ctx, userID)
		if err != nil {
			return nil, err
		}
		out := make([]addressRow, len(entries))
		for i, e := range entries {
			out[i] = addressRow{id: e.ID, cidr: e.CIDR, note: e.Label, source: "user"}
		}
		return out, nil
	}
	h.openAddressManager("allowed IPs — " + who)
}

func (h *Host) openAddressManager(title string) {
	h.addresses.rows, h.addresses.all = nil, nil
	h.addresses.model.Reset(nil)
	h.set("App.addressIndex", 0)
	h.set("App.addressesTitle", title)
	h.set("App.addressAdding", false)
	openManager(h, h.addresses)
	h.open("addresses")
}

func (h *Host) addressesClosed() error {
	h.addresses.bound = nil
	h.set("App.addressAdding", false)
	return nil
}

func (h *Host) addressWritable() bool {
	b := h.addresses.bound
	return b != nil && b.Gen() == h.session.Gen() && b.IdentityEpoch() == h.session.IdentityEpoch() &&
		(!h.addressGlobal || b.User().Role == "admin") && (h.addressGlobal || h.addressUserID != 0)
}

func (h *Host) addressAdd() error {
	if !h.addressWritable() {
		return nil
	}
	h.set("App.addressAdding", true)
	h.set(h.addresses.status, "")
	return nil
}

func (h *Host) addressSave(cidr, note string) error {
	if !h.addressWritable() {
		return nil
	}
	cidr = strings.TrimSpace(cidr)
	if h.addressGlobal {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			h.set(h.addresses.status, "global allowlist requires a CIDR such as 192.0.2.0/24")
			return nil
		}
		cidr = p.Masked().String()
	} else if cidr != "" {
		if p, err := netip.ParsePrefix(cidr); err == nil {
			cidr = p.Masked().String()
		} else if a, err := netip.ParseAddr(cidr); err == nil {
			cidr = netip.PrefixFrom(a, a.BitLen()).String()
		} else {
			h.set(h.addresses.status, "enter an IP or CIDR, or leave blank for this session's address")
			return nil
		}
	}
	userID, global := h.addressUserID, h.addressGlobal
	what := cidr
	if what == "" {
		what = "this session's address"
	}
	managerCall(h, h.addresses, "allow "+what, func(ctx context.Context, b *Bound) error {
		if global {
			return b.AddAllowedIP(ctx, cidr, note)
		}
		return b.AddUserIP(ctx, userID, cidr, note)
	})
	h.set("App.addressAdding", false)
	return nil
}

func (h *Host) addressRemove(i int) error {
	if !h.addressWritable() {
		return nil
	}
	r, ok := h.addresses.at(i)
	if !ok {
		h.set(h.addresses.status, "choose an IP row first")
		return nil
	}
	if r.config {
		h.set(h.addresses.status, "config entries are read-only; edit config.toml and restart")
		return nil
	}
	userID, global := h.addressUserID, h.addressGlobal
	managerCall(h, h.addresses, "remove "+r.cidr, func(ctx context.Context, b *Bound) error {
		if global {
			return b.RemoveAllowedIP(ctx, r.cidr)
		}
		return b.RemoveUserIP(ctx, userID, r.id)
	})
	return nil
}
