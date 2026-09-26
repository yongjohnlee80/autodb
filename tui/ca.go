package tui

import (
	"context"
	"strings"
)

// A developer may distribute the CA certificate even when not an admin.
// Only the certificate CONTENTS belong on this card: a path on the daemon's
// private host helps nobody using the TUI over a tunnel.
func (h *Host) openCA() {
	b := h.session.Bind()
	h.caSeq++
	seq := h.caSeq
	read := h.caFetch
	if read == nil {
		read = func(ctx context.Context, b *Bound) (CAPem, error) { return b.FrontDoorCAPem(ctx) }
	}
	h.setStatus("reading front-door CA certificate…")
	type result struct {
		ca  CAPem
		err error
	}
	do(h, func(ctx context.Context) result {
		ca, err := read(ctx, b)
		return result{ca, err}
	}, func(v result) {
		if seq != h.caSeq || b.Gen() != h.session.Gen() || b.IdentityEpoch() != h.session.IdentityEpoch() {
			return
		}
		if v.err != nil {
			h.setStatus("CA certificate: " + WireErrorMessage(v.err))
			return
		}
		h.caText = ""
		if v.ca.SystemRoots {
			h.set("App.caText", "This install has no private CA: clients verify against their own system roots.\nThere is no private certificate file to distribute.")
		} else {
			if strings.TrimSpace(v.ca.PEM) == "" {
				h.setStatus("CA certificate: the daemon returned an empty document")
				return
			}
			h.caText = v.ca.PEM
			h.set("App.caText", v.ca.PEM)
		}
		h.set("App.caTitle", "front-door CA certificate")
		h.set("App.caCanCopy", h.caText != "")
		h.set("App.caStatus", "Copy the certificate or select and yank · Esc closes")
		h.open("caCert")
	})
}

func (h *Host) caClosed() error {
	h.caText = ""
	h.set("App.caText", "")
	h.set("App.caCanCopy", false)
	return nil
}

func (h *Host) copyCA() error {
	if h.caText == "" {
		return nil
	}
	h.editor.SetRegister(h.caText, false)
	if h.p.App().CopyToClipboard(h.caText) {
		h.set("App.caStatus", "certificate copied to clipboard and editor register")
	} else {
		h.set("App.caStatus", "clipboard unavailable — certificate copied to editor register")
	}
	return nil
}
