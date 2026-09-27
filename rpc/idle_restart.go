package rpc

import (
	"context"

	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
)

// sys.restart_if_idle: stop this daemon only if nothing it would interrupt
// exists, so a frontend that finds it older than the installed binary can
// bring the new one up without cancelling anyone's work. Added with protocol 9.
//
// The decision is the engine's, in one step (core/exec idle_shutdown.go): no
// open transaction, no executing statement, no front-door client connected,
// and — if so — every gate closed before the lock is let go. Admin-only, as
// sys.shutdown is: whether this daemon may be restarted is the session's role,
// never the caller's process lineage.
//
// BUSY IS AN ANSWER, not an error: {"stopping": false, "busy": {...}} carries
// the three counts, because what the frontend does next — say what is running
// and when to retry — depends on them. An error is kept for what an error is:
// not an admin, or another shutdown decision already in progress.
func (s *Server) registerIdleRestart() {
	s.handle("sys.restart_if_idle", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		ident, aerr := s.auth.RequireAdmin(ctx, token)
		if aerr != nil {
			return nil, s.wireErr(aerr)
		}
		counts, owner := s.eng.BeginIdleShutdown()
		if owner == 0 {
			if counts.Busy() {
				// Nothing was refused and nothing closed, so nothing to audit.
				return map[string]any{"stopping": false, "busy": map[string]any{
					"in_transaction": int64(counts.InTransaction),
					"executing":      int64(counts.Executing),
					"wire_sessions":  int64(counts.WireSessions),
				}}, nil
			}
			return nil, &golibrpc.Error{Code: CodeShutdownBlocked,
				Message: "refusing to stop: another shutdown decision is already in " +
					"progress on this server. Retry if it does not complete."}
		}
		// Audited BEFORE the effect, as every privileged act is. If the row
		// cannot land, the decision is undone: ALL THREE gates reopen, or the
		// daemon would refuse every statement and every client for good.
		audit := func() error {
			return s.auth.Audit(ctx, ident.UserID(), peerIP(req), "server_shutdown",
				"idle restart requested over rpc")
		}
		if s.hookShutdownAudit != nil { // the same test seam as sys.shutdown's
			audit = func(next func() error) func() error {
				return func() error { return s.hookShutdownAudit(next) }
			}(audit)
		}
		if err := audit(); err != nil {
			s.eng.AbortIdleShutdown(owner)
			return nil, s.wireErr(err)
		}
		s.RequestShutdown()
		return map[string]any{"stopping": true}, nil
	})
}
