package tui

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	tuidecl "github.com/yongjohnlee80/golib/tui/decl"
)

// Tokens are self-service: the Bound pins the account the list, mint and
// show-once card belong to. Revoked rows are history, hidden by default.
func newTokenManager(h *Host) *manager[PATRow] {
	m := newManager("App.tokensStatus",
		func(ctx context.Context, b *Bound) ([]PATRow, error) { return b.PATs(ctx, b.User().ID) },
		func(r PATRow) tuidecl.Row {
			ips := "inherit"
			if len(r.AllowedIPs) > 0 {
				ips = fmt.Sprint(len(r.AllowedIPs))
			}
			state := "active"
			if r.Revoked {
				state = "revoked"
			}
			return tuidecl.Row{"key": r.Name, "name": r.Name, "expires": r.ExpiresAt,
				"lastUsed": r.LastUsed, "ips": ips, "state": state}
		}, "key", "name", "expires", "lastUsed", "ips", "state")
	m.project = func(rows []PATRow) []PATRow {
		if h.showRevoked {
			return rows
		}
		out := make([]PATRow, 0, len(rows))
		for _, r := range rows {
			if !r.Revoked {
				out = append(out, r)
			}
		}
		return out
	}
	return m
}

func tokenState(h *Host) map[string]any {
	return map[string]any{
		"App.tokenRows":            h.tokens.model,
		"App.tokensStatus":         "",
		"App.showRevokedLabel":     "&Show revoked",
		"App.tokenConnections":     h.tokenConns,
		"App.tokenConnectionIndex": -1,
		"App.tokenFormError":       "",
		"App.tokenFormCleartext":   false,
		"App.tokenFormName":        "", "App.tokenFormDays": "", "App.tokenFormIPs": "", "App.tokenFormDebug": "",
	}
}

func (h *Host) openTokens() {
	h.showRevoked = false
	h.set("App.showRevokedLabel", "&Show revoked")
	h.tokens.all, h.tokens.rows = nil, nil
	h.tokens.model.Reset(nil) // a former identity's rows must not flash on opening
	openManager(h, h.tokens)
	h.open("tokens")
}

func (h *Host) tokensClosed() error {
	h.tokenSeq++
	h.tokenFormBound = nil
	return nil
}

func (h *Host) tokenFormClosed() error {
	if h.tokenFormAccepted {
		h.tokenFormAccepted = false
		return nil
	}
	h.tokenSeq++
	h.tokenFormBound = nil
	return nil
}

func (h *Host) tokenToggleRevoked() error {
	h.showRevoked = !h.showRevoked
	h.tokens.reproject()
	label := "&Show revoked"
	if h.showRevoked {
		label = "&Hide revoked"
	}
	h.set("App.showRevokedLabel", label)
	return nil
}

func (h *Host) tokenRevoke(i int) error {
	r, ok := h.tokens.at(i)
	if !ok {
		h.set(h.tokens.status, "choose a token first")
		return nil
	}
	if r.Revoked {
		h.set(h.tokens.status, r.Name+" is already revoked")
		return nil
	}
	h.confirm("revoke "+r.Name+"?", "The token immediately stops working. Revocation cannot be undone.",
		"&Revoke", "&Keep", func() {
			b := h.tokens.bound
			managerCall(h, h.tokens, "revoke "+r.Name, func(ctx context.Context, _ *Bound) error {
				return b.RevokePAT(ctx, b.User().ID, r.Name)
			})
		})
	return nil
}

func (h *Host) tokenCreate() error {
	b := h.tokens.bound
	if b == nil {
		return nil
	}
	h.tokenSeq++
	seq := h.tokenSeq
	h.tokenFormAccepted = false
	h.set(h.tokens.status, "loading eligible connections…")
	type listed struct {
		conns []ConnInfo
		ep    FrontDoorEndpoint
		err   error
	}
	do(h, func(ctx context.Context) listed {
		conns, err := b.Connections(ctx)
		if err != nil {
			return listed{err: err}
		}
		ep, _ := b.FrontDoorEndpoint(ctx) // failure must not lose a future minted secret
		return listed{conns: conns, ep: ep}
	}, func(v listed) {
		if !h.tokenCurrent(b) || seq != h.tokenSeq {
			return
		}
		if v.err != nil {
			h.set(h.tokens.status, WireErrorMessage(v.err))
			return
		}
		var rows []tuidecl.Row
		for _, c := range v.conns {
			if !c.FrontDoorExposed || c.TargetDB == "" {
				continue
			}
			rows = append(rows, tuidecl.Row{"key": strconv.FormatInt(c.ID, 10),
				"id": strconv.FormatInt(c.ID, 10), "label": c.Name + "  (" + c.Engine + ")"})
		}
		if len(rows) == 0 {
			h.set(h.tokens.status, "no exposed connection with a target database is available")
			return
		}
		h.tokenConns.Reset(rows)
		h.set(h.tokens.status, "")
		h.tokenFormBound = b
		h.tokenAllowCleartext = b.User().Role == "admin" && v.ep.Cleartext
		h.set("App.tokenFormCleartext", h.tokenAllowCleartext)
		h.set("App.tokenFormError", "")
		for _, field := range []string{"App.tokenFormName", "App.tokenFormDays", "App.tokenFormIPs", "App.tokenFormDebug"} {
			h.set(field, " ")
			h.set(field, "")
		}
		h.set("App.tokenConnectionIndex", -1)
		h.set("App.tokenConnectionIndex", 0)
		h.open("tokenForm")
	})
	return nil
}

func (h *Host) tokenCurrent(b *Bound) bool {
	return b != nil && b == h.tokens.bound && b.Gen() == h.session.Gen() &&
		b.IdentityEpoch() == h.session.IdentityEpoch()
}

type mintIntent struct {
	bound  *Bound
	seq    uint64
	name   string
	days   int64
	ips    []string
	connID int64
	debug  bool
}

func (h *Host) refuseTokenForm(reason string) error {
	h.tokenFormAccepted = true // validation reopens; closing this answer is not a cancel
	h.set("App.tokenFormError", reason)
	h.p.Post(func() { h.open("tokenForm") })
	return nil
}

func (h *Host) mintToken(name, daysText, ipsText, connText, cleartext string) error {
	if err := h.p.SetMany(map[string]any{"App.tokenFormName": name, "App.tokenFormDays": daysText,
		"App.tokenFormIPs": ipsText, "App.tokenFormDebug": cleartext}); err != nil {
		h.keep(err)
	}
	b := h.tokenFormBound
	if !h.tokenCurrent(b) {
		return h.refuseTokenForm("the session changed; reopen the token manager")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return h.refuseTokenForm("a token name is required")
	}
	days := int64(0)
	if strings.TrimSpace(daysText) != "" {
		var err error
		days, err = strconv.ParseInt(strings.TrimSpace(daysText), 10, 64)
		if err != nil || days < 1 || days > 365 {
			return h.refuseTokenForm("expires in days must be 1–365")
		}
	}
	connID, err := strconv.ParseInt(connText, 10, 64)
	if err != nil || connID <= 0 {
		return h.refuseTokenForm("choose a connection")
	}
	debug := strings.EqualFold(strings.TrimSpace(cleartext), "yes")
	if cleartext != "" && !debug && !strings.EqualFold(strings.TrimSpace(cleartext), "no") {
		return h.refuseTokenForm("answer yes or no to the cleartext question")
	}
	if debug && !h.tokenAllowCleartext {
		return h.refuseTokenForm("cleartext debug minting is not available here")
	}
	var ips []string
	for _, raw := range strings.Split(ipsText, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if p, err := netip.ParsePrefix(raw); err == nil {
			ips = append(ips, p.Masked().String())
		} else if a, err := netip.ParseAddr(raw); err == nil {
			ips = append(ips, netip.PrefixFrom(a, a.BitLen()).String())
		} else {
			return h.refuseTokenForm("invalid IP/CIDR: " + raw)
		}
	}
	h.tokenFormAccepted = true
	in := mintIntent{bound: b, seq: h.tokenSeq, name: name, days: days, ips: ips, connID: connID, debug: debug}
	h.previewMint(in)
	return nil
}

func (h *Host) previewMint(in mintIntent) {
	preview := h.tokenPreview
	if preview == nil {
		preview = func(ctx context.Context, b *Bound, ips []string) ([]string, error) {
			return b.PATAllowlistPreview(ctx, ips)
		}
	}
	do(h, func(ctx context.Context) struct {
		missing []string
		err     error
	} {
		missing, err := preview(ctx, in.bound, in.ips)
		return struct {
			missing []string
			err     error
		}{missing, err}
	}, func(v struct {
		missing []string
		err     error
	}) {
		h.previewAnswered++
		if !h.tokenCurrent(in.bound) || in.seq != h.tokenSeq {
			return
		}
		if v.err != nil {
			h.set(h.tokens.status, "create "+in.name+": "+WireErrorMessage(v.err))
			return
		}
		if len(v.missing) == 0 {
			h.mintApproved(in, nil)
			return
		}
		h.confirmMintWidening(in, v.missing)
	})
}

func (h *Host) confirmMintWidening(in mintIntent, missing []string) {
	// The approved canonical rows travel to the server; it recomputes them
	// atomically and returns a stale set if the decision has changed.
	body := "To restrict this token, these CIDRs must be added to YOUR standing allowlist:\n  " +
		strings.Join(missing, "\n  ") + "\n\nThey also admit password logins and other inheriting tokens, remain after this token is revoked, and must be removed manually."
	h.confirm("widen your allowlist?", body, "&Add and mint", "&Cancel", func() { h.mintApproved(in, missing) })
}

func (h *Host) mintApproved(in mintIntent, approved []string) {
	if !h.tokenCurrent(in.bound) || in.seq != h.tokenSeq {
		return
	}
	type minted struct {
		out          PATSecret
		stale        []string
		ep           FrontDoorEndpoint
		conns        []ConnInfo
		err          error
		revoked      bool
		compensation error
	}
	mint := h.tokenMint
	if mint == nil {
		mint = func(ctx context.Context, b *Bound, in mintIntent, approved []string) (PATSecret, []string, error) {
			return b.CreatePAT(ctx, in.name, in.days, in.ips, in.connID, in.debug, approved)
		}
	}
	h.mintWorkers.Add(1)
	go func() {
		defer h.mintWorkers.Done()
		// Host cancellation cannot abandon a request whose server may already
		// have committed. Bound the round trip and follow it through to either
		// a card or a compensating revocation before Run returns.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if h.ctx.Err() != nil || in.bound.ensure() != nil {
			return
		}
		out, stale, err := mint(ctx, in.bound, in, approved)
		v := minted{out: out, stale: stale, err: err}
		if errors.Is(err, errMintReplyUncertain) {
			// The server reported success but the one-time answer is malformed.
			// Ordinary create errors never enter this branch: revoking a name
			// after a duplicate-name refusal could revoke an existing token.
			v.revoked, v.compensation = h.compensateMint(in.bound, in.name)
		}
		if err == nil && len(stale) == 0 && out.Name != "" {
			if !in.bound.currentIdentity() || h.ctx.Err() != nil {
				v.revoked, v.compensation = h.compensateMint(in.bound, out.Name)
			} else {
				// Endpoint failure cannot strand the one-time secret; render
				// with an explicit unusable-door warning in that case.
				v.ep, _ = in.bound.FrontDoorEndpoint(ctx)
				v.conns, _ = in.bound.Connections(ctx)
				if !in.bound.currentIdentity() || h.ctx.Err() != nil {
					v.revoked, v.compensation = h.compensateMint(in.bound, out.Name)
				}
			}
		}
		if v.compensation != nil {
			h.recordMintError(in.name, v.compensation)
		}
		if h.ctx.Err() != nil {
			return
		}
		// No live token: status/confirmation is best effort; only a
		// committed usable secret needs an acknowledged UI handoff.
		if v.revoked || v.compensation != nil || len(v.stale) > 0 || v.err != nil {
			h.p.Post(func() {
				if v.revoked || v.compensation != nil {
					h.mintNotice(v.revoked)
					return
				}
				if len(v.stale) > 0 {
					if h.tokenCurrent(in.bound) && in.seq == h.tokenSeq {
						h.confirmMintWidening(in, v.stale)
					}
					return
				}
				if v.err != nil {
					if h.tokenCurrent(in.bound) {
						h.set(h.tokens.status, "create "+in.name+": "+WireErrorMessage(v.err))
					}
					return
				}
			})
			return
		}
		// Posting is NOT delivery. Run may exit without draining this queue,
		// so the worker remains alive until the UI acknowledges the card or
		// shutdown marks the post abandoned and revokes the committed PAT.
		var handoff struct {
			sync.Mutex
			handled    bool
			abandoned  bool
			compensate bool
		}
		delivered := make(chan struct{})
		post := h.postMintHandoff
		if post == nil {
			post = h.p.Post
		}
		post(func() {
			handoff.Lock()
			defer handoff.Unlock()
			if handoff.abandoned || h.ctx.Err() != nil {
				return
			}
			if !h.tokenCurrent(in.bound) || in.seq != h.tokenSeq {
				handoff.compensate = true
			} else if !in.bound.withCurrentIdentity(func() {
				reloadManager(h, h.tokens, "create "+in.name+": ok")
				conn := ConnInfo{ID: in.connID, Name: fmt.Sprintf("connection %d", in.connID)}
				for _, c := range v.conns {
					if c.ID == in.connID {
						conn = c
						break
					}
				}
				h.showConnectionCard(v.out, conn, v.ep, in.bound.User())
			}) {
				handoff.compensate = true
			}
			handoff.handled = true
			close(delivered)
		})
		timer := time.NewTimer(10 * time.Second)
		select {
		case <-delivered:
		case <-h.ctx.Done():
		case <-timer.C:
		}
		timer.Stop()
		handoff.Lock()
		if !handoff.handled {
			handoff.abandoned, handoff.compensate = true, true
		}
		compensate := handoff.compensate
		handoff.Unlock()
		if compensate {
			revoked, err := h.compensateMint(in.bound, v.out.Name)
			if err != nil {
				h.recordMintError(in.name, err)
			}
			if h.ctx.Err() == nil {
				h.p.Post(func() { h.mintNotice(revoked) })
			}
		}
	}()
}

func (h *Host) compensateMint(b *Bound, name string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.revokeMintedAfterSwitch(ctx, name); err != nil {
		return false, err
	}
	return true, nil
}

func (h *Host) recordMintError(name string, err error) {
	h.mintMu.Lock()
	defer h.mintMu.Unlock()
	h.mintErrors = append(h.mintErrors, fmt.Errorf("a newly minted token %q may remain active after sign-in changed; revoke it as its owner: %w", name, err))
}

func (h *Host) mintNotice(revoked bool) {
	if revoked {
		h.setStatus("a token minted during sign-in change was revoked")
	} else {
		h.setStatus("a token may remain active after sign-in change; sign back in as its owner to revoke it")
	}
}
