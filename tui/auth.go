package tui

import (
	"context"
	"fmt"
)

// SIGN-IN — the login and first-run dialogs, and what a sign-in changes.
//
// A terminal program signs its own session in: the server with no users asks
// for its root user (bootstrap), any other asks for a login. The web's session
// arrives signed in by the gateway, shared by the user's tabs, and must never
// be signed in again in place — that would re-key a connection other tabs use —
// so a web program that loses its sign-in ends, and the user signs in again
// through the gateway.
//
// ONE ATTEMPT AT A TIME. Two logins racing would leave whichever answered last
// as the identity. Each attempt carries its own number, and only the current
// one's answer releases the guard: an attempt a reconnect made stale settles
// without effect. Each is pinned to the generation it was issued under, so an
// answer for a connection that no longer exists changes nothing.
//
// A dialog closes when it is answered, as Qt's does. A refused answer — a
// missing user, passphrases that differ, a server that said no — opens it
// again, the reason on its help line.

// authDone is one sign-in attempt's answer.
type authDone struct {
	attempt uint64
	gen     uint64
	what    string // "login" or "bootstrap"
	err     error
}

// login is App.login: the login dialog answered.
func (h *Host) login(user, pass string) error {
	h.set("App.lastUser", user)
	if user == "" {
		h.refuseSignIn("login", "a user is required")
		return nil
	}
	h.authTask("login", func(ctx context.Context, b *Bound) error { return b.Login(ctx, user, pass) })
	return nil
}

// bootstrap is App.bootstrap: the first-run dialog answered. The root user is
// "root" unless named; its passphrase also unlocks the master key.
func (h *Host) bootstrap(user, pass, again string) error {
	if user == "" {
		user = "root"
	}
	switch {
	case pass != again:
		h.refuseSignIn("bootstrap", "the passphrases do not match")
		return nil
	case len(pass) < 8:
		h.refuseSignIn("bootstrap", "a passphrase has at least 8 characters")
		return nil
	}
	h.set("App.lastUser", user)
	h.authTask("bootstrap", func(ctx context.Context, b *Bound) error { return b.Bootstrap(ctx, user, pass) })
	return nil
}

// signInDeclined is App.signInDeclined: a sign-in dialog cancelled. The
// program stays connected and signed out; SPC L asks again.
func (h *Host) signInDeclined() error {
	h.setStatus("not signed in — SPC L signs in")
	return nil
}

// authTask runs one attempt, unless one is already running — then the dialog
// opens again and says so.
func (h *Host) authTask(what string, fn func(context.Context, *Bound) error) {
	if h.authAttempt != 0 {
		h.refuseSignIn(what, "another sign-in is still running — try again in a moment")
		return
	}
	h.authSeq++
	attempt := h.authSeq
	h.authAttempt = attempt
	h.setStatus("signing in…")
	bound := h.session.Bind() // the generation is pinned when the answer is given
	do(h, func(ctx context.Context) authDone {
		return authDone{attempt: attempt, gen: bound.Gen(), what: what, err: fn(ctx, bound)}
	}, h.authSettled)
}

// authSettled applies an attempt's answer, if it is still the current one.
func (h *Host) authSettled(d authDone) {
	if d.attempt == h.authAttempt {
		h.authAttempt = 0
	}
	if d.gen != h.session.Gen() {
		return // signed into a connection that no longer exists
	}
	if d.err != nil {
		h.setStatus(d.what + " failed: " + WireErrorMessage(d.err))
		// A failed first run asks for a login: the server may have gained its
		// root user from another client meanwhile, and the login says so.
		h.refuseSignIn("login", WireErrorMessage(d.err))
		return
	}
	h.afterSignIn()
}

// refuseSignIn opens the dialog for what again, saying why. After the dialog
// has closed: it is still open while its accepted handler runs.
func (h *Host) refuseSignIn(what, why string) {
	h.set("App."+what+"Error", why)
	h.p.Post(func() { h.open(what) })
}

// afterSignIn is a sign-in taking effect: the previous identity is retired
// before the next one's note store exists, so no moment has two usable.
func (h *Host) afterSignIn() {
	u := h.session.User()
	h.hadAuth = true
	h.retireIdentity()
	h.set("App.loginError", "")
	h.set("App.bootstrapError", "")
	h.setAuth("signed-in")
	defer h.refreshIdentity()
	if h.notesFor != nil {
		notes, err := h.notesFor(u.Name)
		if err != nil {
			// Instead of the signed-in line, not before it: signed in with no
			// notes is the one case the user must see.
			h.setStatus(fmt.Sprintf("signed in as %s, but notes are unavailable: %v", u.Name, err))
			h.reloadExplorer()
			return
		}
		h.notes = notes
	}
	h.setStatus(fmt.Sprintf("signed in as %s (%s)", u.Name, u.Role))
	h.reloadExplorer()
}

// retireIdentity ends the current identity's authority: its note store stops
// accepting writes, and nothing it owned carries over.
func (h *Host) retireIdentity() {
	if h.notes != nil {
		h.notes.Retire()
	}
	h.notes = nil
	h.idEpoch++
	h.forgetWorkspace()
}

// promptSignIn opens what sign-in now needs: the first-run dialog or the
// login. The web's program cannot sign in again (see the top of this file), so
// it ends.
func (h *Host) promptSignIn() {
	if !h.ownsConnection() {
		h.endForLostAuth()
		return
	}
	switch h.auth {
	case "bootstrap":
		h.open("bootstrap")
	case "login":
		h.open("login")
	}
}

// promptLogin is session.login (SPC L): sign in, or switch user.
func (h *Host) promptLogin() {
	h.set("App.loginError", "")
	h.open("login")
}

// checkAuth watches for the sign-in going away under a task: the session
// clears its token when the server refuses it (expired, the store relocked),
// whichever call noticed, and the one recovery is to sign in again.
func (h *Host) checkAuth() {
	if h.session == nil {
		return
	}
	if h.session.Token() != "" {
		return
	}
	if !h.hadAuth {
		return
	}
	h.hadAuth = false
	h.retireIdentity()
	h.setStatus("signed out by the server — sign in again")
	h.setAuth("login")
	h.refreshIdentity()
	h.promptSignIn()
}

// endForLostAuth ends a web program whose sign-in is gone: the gateway logs
// the shared session out on its last reference, and the user signs in again
// there. quit ends this browser session only.
func (h *Host) endForLostAuth() {
	h.setStatus("session ended — reload the page to sign in again")
	h.quit()
}
