package exec

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
)

// controlCancelHandler issues PostgreSQL cancellations on a socket charged to
// the reserved control lane rather than to the ordinary connection budget.
//
// WHY IT EXISTS. A cancel is not a message on the session: PostgreSQL requires
// a SECOND connection carrying the backend key, and pgx dials it through the
// same ConnConfig.DialFunc as ordinary work. Once that dialer takes a permit,
// a cancel at full budget is refused — which removes cancellation at precisely
// the moment someone reaches for it, because a developer cancels a query when
// the system is busy, not when it is idle.
//
// pgx's own handler cannot be marked: the context it cancels with descends
// from context.Background(), so nothing autodb sets reaches the dialer. This
// handler replaces it and calls CancelRequest with a context this package has
// marked, which is the only way the dialer can tell the two apart.
//
// It preserves pgx's sequencing deliberately — deadline first, then the cancel
// after a short delay — because that ordering is what lets an in-flight read
// finish before the socket is torn down.
type controlCancelHandler struct {
	conn *pgconn.PgConn

	// cancelDelay mirrors pgx's own: a context cancelled microseconds before a
	// query completes should not spend a socket on a cancel nobody needs.
	cancelDelay time.Duration
	// deadlineDelay is the fallback applied to the net.Conn, so a server that
	// never answers the cancel cannot hold the session open.
	deadlineDelay time.Duration

	finished chan struct{}
	unwatch  context.CancelFunc
}

func (h *controlCancelHandler) HandleCancel(context.Context) {
	h.finished = make(chan struct{})
	var unwatched context.Context
	unwatched, h.unwatch = context.WithCancel(context.Background())

	deadline := time.Now().Add(h.deadlineDelay)
	_ = h.conn.Conn().SetDeadline(deadline)

	go func() {
		defer close(h.finished)
		select {
		case <-unwatched.Done():
			// The statement finished on its own inside the delay. No socket is
			// spent, which is the point of the delay.
			return
		case <-time.After(h.cancelDelay):
		}
		ctx, cancel := context.WithDeadline(unwatched, deadline)
		defer cancel()
		// THE MARKER. Without it this dial is charged to the ordinary
		// allowance and refused at saturation.
		_ = h.conn.CancelRequest(withControlDial(ctx))
	}()
}

func (h *controlCancelHandler) HandleUnwatchAfterCancel() {
	h.unwatch()
	<-h.finished
	_ = h.conn.Conn().SetDeadline(time.Time{})
}

// buildControlCancelHandler installs the handler above on a connection.
func buildControlCancelHandler(conn *pgconn.PgConn) ctxwatch.Handler {
	return &controlCancelHandler{
		conn:          conn,
		cancelDelay:   5 * time.Millisecond,
		deadlineDelay: 5 * time.Second,
	}
}
