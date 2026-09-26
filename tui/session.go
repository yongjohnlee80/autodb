package tui

import (
	"context"
	"fmt"
)

// THE CONNECTION — dialing, redialing, and what it says about sign-in.
//
// A terminal program owns its connection: it dials at start, redials when the
// connection drops, and one transition runs at a time. The web's connection is
// the gateway's, already dialed and signed in and shared by the user's tabs;
// the program enters the same post-connect path at the session's current
// generation, and a lost connection ends it rather than redialing a client
// other tabs are using.
//
// Every result carries the generation it was issued under and is dropped when
// the session has moved on — the rule the whole package keeps.

// startup is the post-connect answer: which generation connected, whether a
// different server answered, and whether it has no users yet.
type startup struct {
	gen             uint64
	instanceChanged bool
	needsBootstrap  bool
	err             error
}

// start begins the program's session: dial, or join the gateway's.
func (h *Host) start() {
	if !h.ownsConnection() {
		h.setStatus("attaching…")
		gen := h.session.Gen()
		do(h, func(context.Context) startup { return startup{gen: gen} }, h.startupDone)
		return
	}
	h.connect()
}

// connect dials, and asks whether the server needs its first user.
func (h *Host) connect() {
	if !h.ownsConnection() || h.connecting {
		return // one transition at a time; the web never redials
	}
	h.connecting = true
	h.setAuth("connecting")
	h.setStatus("connecting to " + h.session.addr + "…")
	sess := h.session
	do(h, func(ctx context.Context) startup {
		changed, err := sess.Connect(ctx)
		if err != nil {
			return startup{err: err}
		}
		// Bound AFTER Connect: this is the one action whose issuance point is
		// the just-installed generation, not the loop-side dispatch.
		bound := sess.Bind()
		needs, err := bound.NeedsBootstrap(ctx)
		if err != nil {
			return startup{err: err}
		}
		return startup{gen: bound.Gen(), instanceChanged: changed, needsBootstrap: needs}
	}, h.startupDone)
}

// startupDone applies a connection's answer, if it is still the current one.
func (h *Host) startupDone(s startup) {
	h.connecting = false
	defer h.refreshIdentity()
	if s.err != nil {
		h.setAuth("disconnected")
		h.setStatus("connect failed: " + s.err.Error())
		return
	}
	if s.gen != h.session.Gen() || !h.session.Connected() {
		// A disconnect or a newer connect superseded this one while its
		// answer was in flight: it must not watch, prompt, or claim a
		// connection that no longer exists.
		h.setAuth("disconnected")
		h.setStatus("connection changed — reconnect")
		return
	}
	h.connectedOnce = true
	h.watch(s.gen)
	if s.instanceChanged {
		h.setStatus("server instance changed — login required")
	} else {
		h.setStatus(fmt.Sprintf("connected — autodb %s", h.session.ServerVersion()))
	}
	switch {
	case s.needsBootstrap:
		h.setAuth("bootstrap")
		h.promptSignIn()
	case h.session.Token() == "":
		h.setAuth("login")
		h.promptSignIn()
	case !h.hadAuth:
		// Signed in already — the web's session, which the gateway signed in
		// — or by a reconnect keeping the token: the identity takes effect
		// the first time only.
		h.afterSignIn()
	default:
		h.setAuth("signed-in")
		h.probeFrontDoorTLS()
	}
}

// watch waits for the connection of generation gen to end, and reacts on the
// loop — if it is still the current one.
func (h *Host) watch(gen uint64) {
	done := h.session.Done()
	if done == nil {
		return // disconnected in the interim; nothing to watch
	}
	sess := h.session
	do(h, func(ctx context.Context) string {
		select {
		case <-done:
		case <-ctx.Done():
			return ""
		}
		if err := sess.Err(); err != nil {
			return err.Error()
		}
		return "connection lost"
	}, func(cause string) {
		if gen != h.session.Gen() {
			return // an old connection's watcher
		}
		h.refreshIdentity()
		if !h.ownsConnection() {
			// The gateway's connection: redialing here would replace the
			// client every other tab uses. This program ends instead.
			h.quit()
			return
		}
		h.setAuth("disconnected")
		h.setStatus("disconnected: " + cause + " — reconnecting…")
		h.connect()
	})
}

// toggleConnection is session.connection_toggle: one command whose label and
// effect follow the connection. A chosen disconnect moves the generation, so
// this connection's watcher stands down and the disconnect stays one.
func (h *Host) toggleConnection() {
	if h.session.Connected() {
		h.session.Disconnect()
		h.clearFrontDoorWarning()
		h.setAuth("disconnected")
		h.setStatus("disconnected — SPC x reconnects")
		h.refreshIdentity()
		return
	}
	h.connect()
}
