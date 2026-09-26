package tui

import (
	"context"
	"fmt"
	"strings"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
)

// Script history comes from the Bound RPC, never from client-side editor
// buffers. The compact QML table delegates all actions to the host.
func newHistoryManager() *manager[HistoryRow] {
	return newManager("App.historyStatus",
		func(ctx context.Context, b *Bound) ([]HistoryRow, error) { return b.History(ctx, 100) },
		func(r HistoryRow) tuidecl.Row {
			status := r.Status
			if r.Suspended {
				status += " (suspended)"
			}
			return tuidecl.Row{"when": r.StartedAt, "who": r.User, "conn": r.Conn,
				"status": status, "rows": r.RowCount, "took": r.Duration.String(),
				"script": strings.ReplaceAll(strings.ReplaceAll(r.Script, "\n", "␤"), "\r", "")}
		}, "when", "who", "conn", "status", "rows", "took", "script")
}

func (h *Host) openHistory() {
	h.history.rows, h.history.all = nil, nil
	h.history.model.Reset(nil)
	h.history.bound = h.session.Bind()
	reloadManager(h, h.history, "Enter script · e load · y copy")
	h.open("history")
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
	h.set("App.valueTitle", fmt.Sprintf("script — %s", r.StartedAt))
	h.set("App.valueText", r.Script)
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
