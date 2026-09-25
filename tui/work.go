package tui

import "context"

// BACKGROUND WORK — off the loop, and back onto it.
//
// Nothing that talks to the backend runs on the UI loop. do runs fn on its own
// goroutine under the host's context, and applies its result on the loop with
// then. The result says which generation it was issued under; then decides
// whether it still applies — a late answer to a question nobody is asking any
// more is dropped there, never here. After it, the host looks at whether the
// sign-in survived the call (auth.go).
func do[T any](h *Host, fn func(context.Context) T, then func(T)) {
	ctx := h.ctx
	go func() {
		v := fn(ctx)
		if ctx.Err() != nil {
			return // the program stopped; nobody is waiting
		}
		h.p.Post(func() {
			then(v)
			// Any call may be the one the server refused the token on.
			h.checkAuth()
		})
	}()
}
