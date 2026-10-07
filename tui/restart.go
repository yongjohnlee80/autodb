package tui

import "context"

// Restart asks the existing server to shut down. The session's disconnect
// watcher owns the subsequent reconnect/spawn; issuing a second dial here
// would race that watcher and create two connection transitions.
func (h *Host) restartConfirmed() error {
	if !h.canRestartDaemon() || !h.session.Connected() || !h.session.IsAdmin() {
		h.statusMessage("autodb.restart.status.status")
		return nil
	}
	b := h.session.Bind()
	call := h.restartCall
	if call == nil {
		call = func(ctx context.Context, b *Bound) error { return b.ShutdownServer(ctx) }
	}
	h.statusMessage("autodb.restart.status.status2")
	do(h, func(ctx context.Context) error { return call(ctx, b) }, func(err error) {
		if b.Gen() != h.session.Gen() || b.IdentityEpoch() != h.session.IdentityEpoch() {
			return
		}
		if err != nil {
			h.setStatus("restart refused: " + WireErrorMessage(err))
			return
		}
		h.statusMessage("autodb.restart.status.status3")
	})
	return nil
}
