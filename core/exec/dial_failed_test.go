package exec

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	golibpg "github.com/yongjohnlee80/golib/dao/postgres"
)

// THE CAUSE MUST NOT BE REACHABLE BY UNWRAPPING, and this is the cell that
// makes that a mechanism rather than a comment.
//
// The front door's extended renderer asks errors.As whether a refusal is a
// *pgconn.PgError and, if it is, forwards it to the client field for field —
// Message, Detail, Hint, SchemaName, the lot. That arm is correct for a target
// error raised by the client's own statement. An upstream authentication
// failure is also a *pgconn.PgError, and it names the role autodb connects as;
// if a dial failure unwrapped to it, that arm would find it and publish it.
//
// So a failure carrying a PgError cause must answer NO to errors.As, and the
// only route to the cause is Cause, which nothing on the wire path calls.
func TestDialFailure_TheCauseIsNotReachableByUnwrapping(t *testing.T) {
	t.Parallel()

	upstream := &pgconn.PgError{
		Severity: "FATAL", Code: "28P01",
		Message: `password authentication failed for user "autodb"`,
		Detail:  "Connection matched pg_hba.conf line 96",
	}
	f := NewDialFailure(upstream)

	var found *pgconn.PgError
	if errors.As(error(f), &found) {
		t.Fatalf("errors.As found the upstream PgError %+v inside a dial failure; the "+
			"front door's target-error arm walks the chain with errors.As and would "+
			"forward it to the client verbatim", found)
	}
	if !errors.Is(f, ErrDialFailed) {
		t.Error("a dial failure does not answer for its own sentinel, so no renderer can " +
			"recognise one")
	}
	if got, ok := DialFailureOf(error(f)); !ok || got.Cause() != error(upstream) {
		t.Error("Cause does not return the raw error; the operator's half of the contract " +
			"is that the trail keeps what the wire discards")
	}
	if !strings.Contains(f.AuditDetail(), "28P01") {
		t.Errorf("the audit detail %q does not carry the upstream cause", f.AuditDetail())
	}
	if !strings.Contains(f.AuditDetail(), "stage=authenticate") {
		t.Errorf("the audit detail %q does not name the stage; DNS, TLS and an upstream "+
			"password are three different repairs", f.AuditDetail())
	}
}

// A stage is attributed from the error the driver actually produced, and the
// nesting is the part that goes wrong: a TLS failure arrives wrapped in a
// *net.OpError, so a classifier that tests the transport first files every
// expired certificate as a firewall problem.
func TestDialFailure_StagesAreAttributedThroughTheNesting(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name  string
		cause error
		want  DialStage
	}{
		{"a name that will not resolve", &net.DNSError{Err: "no such host", Name: "db.example"}, DialStageResolve},
		{"a socket that will not open",
			&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, DialStageConnect},
		{"an unknown certificate authority, wrapped in a dial",
			&net.OpError{Op: "dial", Net: "tcp", Err: x509.UnknownAuthorityError{}}, DialStageTLS},
		{"a TLS alert, wrapped in a dial",
			&net.OpError{Op: "remote error", Net: "tcp", Err: tls.AlertError(48)}, DialStageTLS},
		{"a hostname the certificate does not cover",
			x509.HostnameError{Host: "db.example"}, DialStageTLS},
		{"an upstream password", &pgconn.PgError{Code: "28P01"}, DialStageAuthenticate},
		{"an upstream database that does not exist", &pgconn.PgError{Code: "3D000"}, DialStageAuthenticate},
		{"an upstream refusal that is neither", &pgconn.PgError{Code: "53300"}, DialStageStartup},
		{"a deadline", fmt.Errorf("dialing: %w", context.DeadlineExceeded), DialStageConnect},
		{"something nobody taught this", errors.New("a driver said something new"), DialStageUnclassified},
	} {
		if got := NewDialFailure(c.cause).Stage; got != c.want {
			t.Errorf("%s: stage = %q, want %q", c.name, got, c.want)
		}
	}
}

// A PERMANENTLY FAILING TARGET COSTS EXACTLY TWO ARBITRATIONS.
//
// One grant, then exactly one re-arbitration. The bound is the requirement: a
// permit is an instance-wide allowance, so an unbounded retry against a target
// that is simply down would take the budget every other session is queued for
// and hand it back, over and over, while making no progress at all. The single
// retry is there because the first failure genuinely can be a permit granted
// against a backend that has just gone away.
func TestDialFailure_OneReArbitrationMaximum(t *testing.T) {
	t.Parallel()

	attempts := 0
	_, err := acquireWithReArbitration(context.Background(),
		func() (golibpg.PinnedConn, error) {
			attempts++
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		})
	if attempts != 2 {
		t.Errorf("a permanently failing target was dialled %d times, want exactly 2 — "+
			"one grant and one re-arbitration", attempts)
	}
	d, ok := DialFailureOf(err)
	if !ok {
		t.Fatalf("the acquisition returned %v, want a dial failure", err)
	}
	if d.Attempts != 2 {
		t.Errorf("the failure reports %d attempts, want 2; an operator reading a trail "+
			"full of these cannot otherwise tell one failure from a retry loop", d.Attempts)
	}

	// A SECOND ATTEMPT THAT SUCCEEDS IS THE WHOLE REASON THE FIRST RETRY
	// EXISTS. Without this the bound could be satisfied by never retrying at
	// all, and the cell above would still pass.
	attempts = 0
	_, err = acquireWithReArbitration(context.Background(),
		func() (golibpg.PinnedConn, error) {
			attempts++
			if attempts == 1 {
				return nil, errors.New("the backend went away between the grant and the dial")
			}
			return nil, nil // a nil conn is enough: the cell is about the control flow
		})
	if err != nil {
		t.Errorf("a target that answered on the second attempt still failed: %v", err)
	}
	if attempts != 2 {
		t.Errorf("the second attempt was made %d times, want 1 retry", attempts-1)
	}
}

// A REQUEST THE CALLER HAS ALREADY ABANDONED IS NOT A TARGET OUTAGE.
//
// Two separate things must hold, and the second is the one this cell exists
// for. The request is not re-arbitrated, because spending a second permit — an
// instance-wide allowance other sessions are queued for — on work nobody is
// waiting for takes capacity from a session that is. And the error that comes
// back IS the cancellation, not a dial failure wearing one as its cause.
//
// What goes wrong when the second half is missing is entirely on the operator's
// side, which is why it survived so long: the retry stops, so nothing looks
// wrong from the client's seat, and meanwhile every client that presses Ctrl-C
// writes a target-outage event and a target-outage frame. An operator watching
// that trail sees a database that cannot be reached and goes hunting a network
// fault that never existed.
//
// THE TWO PREDICATES ASSERTED HERE ARE THE ONES THE FRONT DOOR ACTUALLY ASKS.
// DialFailureOf is what decides whether the event stream gets a dial-failed
// record instead of an ordinary refusal, and errors.Is against the sentinel is
// what selects the retryable target-failure frame and its fixed literal. An
// error that answers no to both cannot produce either, so proving these two is
// proving the acceptance rather than approximating it.
func TestDialFailure_ACancelledRequestIsNotATargetOutage(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	attempts := 0
	_, err := acquireWithReArbitration(ctx, func() (golibpg.PinnedConn, error) {
		attempts++
		// A REAL DIAL ERROR, not context.Canceled. If the attempt returned the
		// cancellation itself, a wrap that was still happening would produce a
		// failure whose cause answered errors.Is for context.Canceled through
		// no mechanism of this function's, and the cell would pass while the
		// contradiction stood.
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	})
	if attempts != 1 {
		t.Errorf("an abandoned request was dialled %d times, want 1", attempts)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled request returned %v, want an error that answers for "+
			"context.Canceled — the caller stopped waiting and that is what happened", err)
	}
	if errors.Is(err, ErrDialFailed) {
		t.Errorf("a cancelled request answers for the dial-failed sentinel, so the front "+
			"door renders it as a retryable target failure and tells the client the "+
			"database is unreachable: %v", err)
	}
	if d, ok := DialFailureOf(err); ok {
		t.Errorf("a cancelled request carries a dial failure (%s), so the front door's "+
			"event builder writes a target-outage record for work the caller itself "+
			"stopped", d.AuditDetail())
	}
}

// A CANCELLATION CARRYING A CAUSE REPORTS THE CAUSE.
//
// A front door that cancels a request for a reason of its own — an expired
// lease, a session closed underneath the statement — has already written that
// reason down. Returning the bare sentinel would throw away the only
// description of what actually happened, and the operator would be left with
// "canceled" for every one of them.
func TestDialFailure_ACancellationReportsTheCauseTheCallerAttached(t *testing.T) {
	t.Parallel()

	reason := errors.New("the session was closed while the statement was in flight")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(reason)

	_, err := acquireWithReArbitration(ctx, func() (golibpg.PinnedConn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	})
	if !errors.Is(err, reason) {
		t.Errorf("the acquisition returned %v, want the cause the caller attached", err)
	}
	if errors.Is(err, ErrDialFailed) {
		t.Errorf("a cancellation with a cause is still framed as a target outage: %v", err)
	}
}

// AN EXPIRED REQUEST IS THE SAME CASE AS A CANCELLED ONE.
//
// A deadline that passed is the caller's clock running out, not the target
// failing to answer — and the stage classifier files context.DeadlineExceeded
// as "connect", so a deadline that reached the wrap below would be reported as
// a transport that would not open. That is a fact about our topology that is
// simply untrue, and an operator would go and test a socket that is healthy.
func TestDialFailure_AnExpiredRequestIsNotATargetOutage(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	attempts := 0
	_, err := acquireWithReArbitration(ctx, func() (golibpg.PinnedConn, error) {
		attempts++
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	})
	if attempts != 1 {
		t.Errorf("an expired request was dialled %d times, want 1", attempts)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("an expired request returned %v, want context.DeadlineExceeded", err)
	}
	if errors.Is(err, ErrDialFailed) {
		t.Errorf("an expired request is framed as a target outage: %v", err)
	}
}
