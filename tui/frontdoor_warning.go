package tui

import "context"

// The daemon's LIVE endpoint, never the frontend's local config, decides
// whether access tokens are crossing an unencrypted listener. A failed probe
// makes no claim rather than teaching a false warning.
func (h *Host) probeFrontDoorTLS() {
	if h.session.Token() == "" {
		return
	}
	h.frontDoorSeq++
	seq := h.frontDoorSeq
	b := h.session.Bind()
	read := h.frontDoorProbe
	if read == nil {
		read = func(ctx context.Context, b *Bound) (FrontDoorEndpoint, error) { return b.FrontDoorEndpoint(ctx) }
	}
	type result struct {
		ep  FrontDoorEndpoint
		err error
	}
	do(h, func(ctx context.Context) result {
		ep, err := read(ctx, b)
		return result{ep, err}
	}, func(v result) {
		if seq != h.frontDoorSeq || b.Gen() != h.session.Gen() || b.IdentityEpoch() != h.session.IdentityEpoch() {
			return
		}
		if v.err != nil {
			return
		}
		h.setFrontDoorCleartext(v.ep.Configured() && v.ep.Cleartext)
	})
}

func (h *Host) setFrontDoorCleartext(on bool) {
	if on == h.cleartextFD {
		return
	}
	h.cleartextFD, h.cleartextSeen = on, false
	if on {
		h.setStatus("FRONT DOOR IS SERVING WITHOUT TLS — access tokens cross the network in cleartext. SPC ! dismisses this.")
	}
	h.refreshIdentity()
}

func (h *Host) clearFrontDoorWarning() {
	h.frontDoorSeq++
	h.cleartextFD, h.cleartextSeen = false, false
	if h.p != nil {
		h.refreshIdentity()
	}
}

func (h *Host) backendWithWarning() string {
	if h.cleartextFD && !h.cleartextSeen {
		return "!! NO TLS !!  " + backendText(h.session)
	}
	return backendText(h.session)
}

func (h *Host) dismissCleartextWarning() {
	if !h.cleartextFD {
		h.setStatus("no cleartext warning to dismiss")
		return
	}
	h.cleartextSeen = true
	h.refreshIdentity() // the warning and its leader entry vanish together
	h.setStatus("cleartext warning dismissed for this session")
}
