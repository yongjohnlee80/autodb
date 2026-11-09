package frontdoor

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"time"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// THE ADMISSION PHASE: an authorized login waits for its pool, outside the
// credential phase.
//
// The credential phase has ended by the time this runs: its auth worker is back,
// its pre-auth slot is back, and its deadline no longer applies. The login waits
// in its pool's line for up to the engine's queue wait (90 s), then the engine
// finalizes it. While it waits nothing reads the socket, so a watch does: a
// client that hangs up leaves the line at once, and one that speaks before it
// was told it may is refused, never served.

// errAdmitPeerGone ends an admission whose client left while it waited.
var errAdmitPeerGone = errors.New("frontdoor: the client left while its login was being admitted")

// runAdmit admits an authorized login and reports how the phase ended.
//
// admitted is the listener's ownership slot: it is set the moment the engine
// hands back an open session, before anything else can fail, so the listener's
// guard closes it on every exit until the session loop takes it.
func (l *Listener) runAdmit(conn net.Conn, fr *frameReader, authz *exec.WireAuthorization,
	peer string, admitted **exec.WireSessionResult) (authOutcome, error) {
	// INPUT ALREADY HERE IS INPUT SENT TOO EARLY. The credential phase gave
	// pgproto3 exactly the password frame, so anything the reader holds now
	// arrived before the login was told it may speak.
	if fr != nil && fr.buffered() {
		return authOutcome{Denied: reasonPreAuthProtocolViolation}, nil
	}
	w := l.watchClient(conn, fr)
	res, aerr := authz.Admit(w.ctx)
	if aerr == nil {
		*admitted = &res
	}
	if h := l.testAfterAdmit; h != nil {
		h()
	}
	got := w.stop()
	// The answer, success or refusal, is written after the wait under its own
	// budget: the credential deadline that bounded the socket is long past.
	_ = conn.SetDeadline(l.now().Add(l.dl.reply))

	switch got {
	case watchProtocolInput:
		l.closeAdmitted(admitted, peer, "protocol-violation")
		return authOutcome{Denied: reasonPreAuthProtocolViolation}, nil
	case watchPeerGone, watchSetupFailed:
		l.closeAdmitted(admitted, peer, "peer-closed")
		return authOutcome{Failure: outcomeID(OutcomeAdmitPeerGone)}, errAdmitPeerGone
	}
	if aerr != nil {
		if errors.Is(aerr, exec.ErrClientGone) {
			return authOutcome{Failure: outcomeID(OutcomeAdmitPeerGone)}, aerr
		}
		return l.engineOutcome(aerr, peer)
	}
	return authOutcome{Session: res}, nil
}

// closeAdmitted closes the session the admission opened, if it did, and clears
// the listener's ownership slot so nothing closes it twice.
func (l *Listener) closeAdmitted(admitted **exec.WireSessionResult, peer, reason string) {
	if *admitted == nil {
		return
	}
	s := *admitted
	*admitted = nil
	l.authn.CloseWireSession(context.Background(), s.SessionID, s.UserID, hostOf(peer), reason)
}

// watchResult is how the admission watch ended. stop() always returns one.
type watchResult int

const (
	watchStopped       watchResult = iota // stop() knocked; nothing arrived
	watchPeerGone                         // EOF or reset: the client left
	watchProtocolInput                    // a byte arrived before AuthenticationOk
	watchSetupFailed                      // the credential deadline could not be cleared
)

// clientWatch watches an admitted login's socket while it waits. ctx is the
// admission context: it is cancelled, with cause exec.ErrClientGone, the moment
// the watch ends for any reason but stop().
type clientWatch struct {
	ctx     context.Context
	cancel  context.CancelCauseFunc
	done    chan watchResult
	knock   func()
	knocked atomic.Bool
}

// watchClient starts the watch: one blocking read of one byte, through the
// frame reader, so a byte it reads stays buffered as evidence rather than lost.
func (l *Listener) watchClient(conn net.Conn, fr *frameReader) *clientWatch {
	ctx, cancel := context.WithCancelCause(context.Background())
	w := &clientWatch{ctx: ctx, cancel: cancel, done: make(chan watchResult, 1), knock: knockFor(conn, l.now)}
	seen := l.testWatchRead
	// The credential deadline is still armed on the socket and must not end the
	// wait; the wait's bound is the engine's.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		w.done <- watchSetupFailed
		cancel(exec.ErrClientGone)
		return w
	}
	go func() {
		var (
			n   int
			err error
		)
		if fr != nil {
			n, err = fr.waitByte()
		} else {
			var b [1]byte
			n, err = conn.Read(b[:])
		}
		if seen != nil {
			seen(n)
		}
		// ONLY THE KNOCK'S OWN TIMEOUT IS A STOP. A client that hung up as the
		// knock landed ends the read with EOF or a reset, not a deadline, and
		// must not be mistaken for a normal stop and handed a session.
		var ne net.Error
		switch {
		case n > 0:
			w.done <- watchProtocolInput
		case w.knocked.Load() && errors.As(err, &ne) && ne.Timeout():
			w.done <- watchStopped
		default:
			w.done <- watchPeerGone
		}
		cancel(exec.ErrClientGone)
	}()
	return w
}

// stop knocks the watch's read loose and joins it, so the socket has one reader
// again before anything is written. knockFor closes the transport if the read
// deadline cannot be set, so the join cannot hang.
func (w *clientWatch) stop() watchResult {
	select {
	case got := <-w.done: // the watch already ended on its own
		return got
	default:
	}
	w.knocked.Store(true)
	w.knock()
	return <-w.done
}
