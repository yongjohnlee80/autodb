package frontdoor

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/core/timeouts"
)

// The front door's half of a login that waits for its pool after
// authentication: the credential boundary, the client watch, the two phases and
// the handoff of an admitted session. Every ordering is forced by a hook, never
// inferred from a sleep.

// pgConnect opens a real pgconn client to a front-door address.
func pgConnect(ctx context.Context, addr, secret, database string) (*pgconn.PgConn, error) {
	host, port, ok := strings.Cut(addr, ":")
	if !ok {
		return nil, fmt.Errorf("listener address %q is not host:port", addr)
	}
	cfg, err := pgconn.ParseConfig(fmt.Sprintf("postgres://root:%s@%s:%s/%s?sslmode=require", secret, host, port, database))
	if err != nil {
		return nil, err
	}
	cfg.TLSConfig.InsecureSkipVerify = true // the cell's own self-signed listener
	return pgconn.ConnectConfig(ctx, cfg)
}

// shareOfOne shrinks the default pool to 3, so its front-door share is one
// session: one busy holder fills it.
func shareOfOne(t *testing.T, l pgLoopHandles) {
	t.Helper()
	if err := l.eng.UpdatePool(opCtx(t), l.rootTok, meta.DefaultPoolID, 3, "127.0.0.1"); err != nil {
		t.Fatalf("shrinking the default pool to 3: %v", err)
	}
}

// busyHolder logs in on addr and keeps its session busy for d (a running
// statement is never reclaimable), returning a channel closed when it finishes.
func busyHolder(t *testing.T, addr, secret, database string, d time.Duration) <-chan struct{} {
	t.Helper()
	c, err := pgConnect(opCtx(t), addr, secret, database)
	if err != nil {
		t.Fatalf("the holder's login: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.Exec(context.Background(), fmt.Sprintf("SELECT pg_sleep(%g)", d.Seconds())).ReadAll()
	}()
	return done
}

// A LOGIN OUTWAITS THE OLD SIX SECONDS. The share is held by a statement that
// runs 8 s; a second login waits in line past the credential phase's whole
// budget, is served when the holder finishes, and runs its first statement.
func TestAdmitFD_ALoginOutwaitsTheOldSixSeconds(t *testing.T) {
	l := pgLoopFull(t)
	shareOfOne(t, l)
	held := busyHolder(t, l.addr, l.secret, l.database, 8*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := time.Now()
	c, err := pgConnect(ctx, l.addr, l.secret, l.database)
	if err != nil {
		t.Fatalf("a login queued behind an 8 s statement was refused after %v: %v; the wait "+
			"must outlast the credential phase", time.Since(start), err)
	}
	defer c.Close(context.Background())
	<-held
	if _, err := c.Exec(ctx, "SELECT 1").ReadAll(); err != nil {
		t.Fatalf("the served login could not run its first statement: %v", err)
	}
}

// A WAITING LOGIN HOLDS NO AUTH WORKER. With one worker, a login queued behind
// a full share must not stop the next client from authenticating: the second
// client also reaches the line while the first still waits.
func TestAdmitFD_AWaitingLoginHoldsNoAuthWorker(t *testing.T) {
	queued := make(chan struct{}, 8)
	l := pgLoopFull(t, exec.WithWaiterQueued(func() { queued <- struct{}{} }))
	shareOfOne(t, l)
	_, _, addr := listenerWith(t, Options{Authn: l.eng, Queries: l.eng, AuthFailuresPerIP: unthrottled, AuthWorkers: 1})
	busyHolder(t, addr, l.secret, l.database, 6*time.Second)

	for i := 0; i < 2; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if c, err := pgConnect(ctx, addr, l.secret, l.database); err == nil {
				_ = c.Close(context.Background())
			}
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-queued:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 2 logins reached the line with one auth worker: a waiting "+
				"login is still holding the worker its credential check used", i)
		}
	}
}

// loginRaw starts a login on addr and answers the password prompt with frames
// written in ONE write: the password, then extra. It returns what the server
// said before it closed, read until EOF.
type rawAnswer struct {
	authOK, ready bool
	errCode       string
}

func loginRaw(t *testing.T, addr, secret, database string, extra []byte, beforePassword, afterPassword func(*tls.Conn)) rawAnswer {
	t.Helper()
	conn, fe := startupTo(t, addr, map[string]string{"user": "root", "database": database})
	t.Cleanup(func() { _ = conn.Close() })
	if beforePassword != nil {
		beforePassword(conn)
	}
	if _, err := fe.Receive(); err != nil {
		t.Fatalf("the password prompt: %v", err)
	}
	pw, err := (&pgproto3.PasswordMessage{Password: secret}).Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(pw, extra...)); err != nil {
		t.Fatalf("writing the password: %v", err)
	}
	if afterPassword != nil {
		afterPassword(conn)
	}
	var got rawAnswer
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	for {
		msg, err := fe.Receive()
		if err != nil {
			return got
		}
		switch m := msg.(type) {
		case *pgproto3.AuthenticationOk:
			got.authOK = true
		case *pgproto3.ReadyForQuery:
			got.ready = true
			return got
		case *pgproto3.ErrorResponse:
			got.errCode = m.Code
		}
	}
}

// INPUT SENT BEFORE THE LOGIN MAY SPEAK IS REFUSED, NEVER SERVED.
//
// Coalesced with the password in one TCP write -- a whole Query, or a single
// trailing type byte -- and, separately, a byte sent while the login waits.
// Each ends with the protocol refusal before readiness. The coalesced cases are
// what the credential boundary is for: without it pgproto3 would read the extra
// bytes ahead into its own buffer, where nothing else can see them.
func TestAdmitFD_InputSentBeforeTheLoginMaySpeakIsRefused(t *testing.T) {
	query, err := (&pgproto3.Query{String: "SELECT 1"}).Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		extra []byte
	}{
		{"a whole Query coalesced with the password", query},
		{"one trailing type byte coalesced with the password", []byte{'Q'}},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := pgLoopFull(t)
			got := loginRaw(t, l.addr, l.secret, l.database, c.extra, nil, nil)
			if got.authOK || got.ready {
				t.Fatalf("the login was admitted (AuthenticationOk %v, ReadyForQuery %v) though it "+
					"sent input with its password", got.authOK, got.ready)
			}
		})
	}
	t.Run("a byte sent while the login waits", func(t *testing.T) {
		queued := make(chan struct{}, 4)
		l := pgLoopFull(t, exec.WithWaiterQueued(func() { queued <- struct{}{} }))
		shareOfOne(t, l)
		busyHolder(t, l.addr, l.secret, l.database, 4*time.Second)
		got := loginRaw(t, l.addr, l.secret, l.database, nil, nil, func(conn *tls.Conn) {
			select {
			case <-queued:
			case <-time.After(10 * time.Second):
				t.Fatal("the login never reached the line")
			}
			_, _ = conn.Write([]byte{'Q'})
		})
		if got.authOK || got.ready {
			t.Fatal("a login that spoke while it waited was admitted")
		}
	})
}

// THE WATCH'S RESULT DECIDES AT THE HANDOFF. The engine has handed over an open
// session; before the watch is joined, the client sends a byte (or hangs up).
// The byte must be refused as protocol input with no readiness written, the
// hang-up must end silently, and either way the admitted session is closed.
func TestAdmitFD_TheWatchDecidesAtTheHandoff(t *testing.T) {
	for _, c := range []struct {
		name   string
		act    func(*tls.Conn)
		reason string
	}{
		{"a byte arrives", func(conn *tls.Conn) { _, _ = conn.Write([]byte{'Q'}) }, "protocol-violation"},
		{"the client hangs up", func(conn *tls.Conn) { _ = conn.Close() }, "peer-closed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := pgLoopFull(t)
			var client atomic.Pointer[tls.Conn]
			read := make(chan struct{}, 1)
			_, _, addr := listenerWith(t, Options{
				Authn: l.eng, Queries: l.eng, AuthFailuresPerIP: unthrottled,
				testAfterAdmit: func() {
					c.act(client.Load())
					select {
					case <-read:
					case <-time.After(5 * time.Second):
					}
				},
				testWatchRead: func(int) {
					select {
					case read <- struct{}{}:
					default:
					}
				},
			})
			got := loginRaw(t, addr, l.secret, l.database, nil, func(conn *tls.Conn) { client.Store(conn) }, nil)
			if got.authOK || got.ready {
				t.Fatalf("an admitted login whose watch lost was handed over (AuthenticationOk %v, "+
					"ReadyForQuery %v)", got.authOK, got.ready)
			}
			waitFor(t, "the admitted session's close", func() bool {
				return closedFor(t, l, c.reason) == 1
			})
		})
	}
}

// closedFor counts session_closed audit rows ending with reason.
func closedFor(t *testing.T, l pgLoopHandles, reason string) int {
	t.Helper()
	rows, err := l.store.Audit.OnCtx(context.Background()).With(meta.AuditAction, "session_closed").Select()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if strings.HasSuffix(r.Detail, reason) {
			n++
		}
	}
	return n
}

// AN ANSWER AFTER THE WAIT IS WRITTEN, THOUGH THE CREDENTIAL DEADLINE HAS PASSED.
// The admission takes longer than the whole credential budget, then ends as a
// finalization timeout: the client still receives the startup-unavailable frame
// (57P03), written under the reply budget.
func TestAdmitFD_AnAnswerAfterTheWaitIsWritten(t *testing.T) {
	dl := defaultDeadlines()
	dl.auth = 300 * time.Millisecond
	slow := &slowAdmit{wait: 3 * dl.auth}
	_, _, addr := listenerWith(t, Options{Authn: slow, AuthFailuresPerIP: unthrottled, testDeadlines: &dl})
	got := loginRaw(t, addr, "irrelevant", "lm-prod", nil, nil, nil)
	if got.errCode != "57P03" {
		t.Fatalf("an admission that ended after the credential deadline answered %q, want 57P03 "+
			"written under the reply budget (%v)", got.errCode, timeouts.HandshakeReserve)
	}
}

// slowAdmit authorizes at once and then takes wait to fail its admission with
// a finalization timeout.
type slowAdmit struct{ wait time.Duration }

func (s *slowAdmit) AuthorizeWire(context.Context, exec.WireOpen) (*exec.WireAuthorization, error) {
	return exec.PreAdmitted(func(ctx context.Context) (exec.WireSessionResult, error) {
		select {
		case <-time.After(s.wait):
		case <-ctx.Done():
			return exec.WireSessionResult{}, ctx.Err()
		}
		return exec.WireSessionResult{}, exec.ErrAdmitFinalizeTimeout
	}), nil
}

func (s *slowAdmit) CloseWireSession(context.Context, exec.SessionID, int64, string, string) {}
