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

// A REQUEST THE CALLER HAS ALREADY ABANDONED IS NOT RETRIED. Spending a second
// permit on work nobody is waiting for takes capacity from a session that is,
// and it would report the target as unreachable when the only thing that ended
// was the request.
func TestDialFailure_ACancelledRequestIsNotReArbitrated(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	attempts := 0
	_, err := acquireWithReArbitration(ctx, func() (golibpg.PinnedConn, error) {
		attempts++
		return nil, context.Canceled
	})
	if attempts != 1 {
		t.Errorf("an abandoned request was dialled %d times, want 1", attempts)
	}
	d, ok := DialFailureOf(err)
	if !ok {
		t.Fatalf("the acquisition returned %v, want a dial failure", err)
	}
	if d.Attempts != 1 {
		t.Errorf("the failure reports %d attempts, want 1", d.Attempts)
	}
}
