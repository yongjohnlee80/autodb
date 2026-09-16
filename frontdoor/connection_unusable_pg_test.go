package frontdoor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// THE HALF THAT DECIDES F0000: A REAL DRIVER, A REAL TARGET, AND A SESSION
// THAT IS STILL USABLE AFTERWARDS.
//
// The unit cell beside this one proves the frame is what this package says it
// is. It cannot prove what a driver DOES with the code, and the choice turns
// entirely on that: a code the driver treats as connection-fatal converts
// "this connection is misconfigured" into "your session died" without a single
// frame being wrong. Class F0 is not in any client's or pool's recovery
// convention, which is the argument — and an argument is not a measurement,
// which is why this cell exists.
//
// THE JDBC HALF HAS NOT BEEN RUN for this shape. The dial-failure shape has a
// written manual procedure and this one does not yet, so F0000 rests on one
// client of the two and the comment in denial.go must not claim otherwise.

const configFaultMarker = "/*config-fault*/"

func configFaultArmed(sqlText string) bool { return strings.Contains(sqlText, configFaultMarker) }

// configFaultQueries is the real engine with one injectable condition: the
// connection this request needs cannot serve it as configured.
type configFaultQueries struct {
	*exec.Engine
}

// configFaultCause names a connection, a role, a host and a port, so the cell
// can prove none of them reaches the client.
func configFaultCause() error {
	return errors.New(`exec: opening connection "billing-prod": parse "postgres://` +
		`autodb_rw@db7.internal:6432/billing": invalid port`)
}

func (q *configFaultQueries) WireQuery(ctx context.Context, id exec.SessionID, userID int64,
	sql, ip string, emit func(exec.WireMessage) error) (byte, error) {

	if configFaultArmed(sql) {
		return 0, exec.NewConfigFailure(exec.ConfigStagePool, 7, exec.DetailPoolRefused, configFaultCause())
	}
	return q.Engine.WireQuery(ctx, id, userID, sql, ip, emit)
}

func TestConnectionUnusablePG_PgxKeepsTheSessionAndLearnsNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h := pgLoopFull(t)
	q := &configFaultQueries{Engine: h.eng}
	_, events, addr := listenerWith(t, Options{
		Authn: h.eng, Queries: q, AuthFailuresPerIP: unthrottled,
	})
	conn := dialFaultConn(t, ctx, addr, h.secret, h.database)

	_, err := conn.Exec(ctx, "SELECT 1 "+configFaultMarker).ReadAll()
	if err == nil {
		t.Fatal("the armed configuration fault produced no error")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("pgx reported %T (%v), want a *pgconn.PgError — a driver that cannot parse "+
			"this as a server error will treat it as a transport failure", err, err)
	}
	if pgErr.Code != ConnectionUnusableSQLState {
		t.Errorf("the driver read SQLSTATE %q, want %q", pgErr.Code, ConnectionUnusableSQLState)
	}
	if pgErr.Severity != "ERROR" {
		t.Errorf("the driver read severity %q, want ERROR", pgErr.Severity)
	}
	for _, tok := range []string{"billing-prod", "autodb_rw", "db7.internal", "6432"} {
		for _, field := range []string{pgErr.Message, pgErr.Detail, pgErr.Hint} {
			if strings.Contains(strings.ToLower(field), strings.ToLower(tok)) {
				t.Errorf("the driver read %q out of the frame; this install's own "+
					"configuration reached a client", tok)
			}
		}
	}

	if conn.IsClosed() {
		t.Fatal("pgx closed the connection on F0000; the session is supposed to survive, " +
			"and a code the driver treats as connection-fatal breaks that promise without " +
			"a single frame being wrong")
	}

	// THE SESSION IS STILL A SESSION. Real work, same connection, real target.
	res, err := conn.Exec(ctx, "SELECT 40 + 2 AS n").ReadAll()
	if err != nil {
		t.Fatalf("the statement after the configuration failure failed: %v", err)
	}
	if len(res) != 1 || len(res[0].Rows) != 1 || string(res[0].Rows[0][0]) != "42" {
		t.Fatalf("the statement after the configuration failure returned %+v, want one row of 42", res)
	}

	// THE TRAIL SAYS WHICH CHECK FAILED AND ON WHICH CONNECTION, AND NOTHING
	// THE CONNECTION STRING CARRIES. This cell used to require the raw cause
	// here; Event.Detail is published to whatever consumes the event stream,
	// and the cause names the host and can name a credential.
	var detail string
	for _, ev := range events() {
		if ev.Kind == EventConnectionUnusable {
			detail = ev.Detail
		}
	}
	if detail == "" {
		t.Fatal("the operator's trail has no connection-unusable event; the client is " +
			"denied the detail on the understanding that the operator is not")
	}
	for _, want := range []string{"stage=" + string(exec.ConfigStagePool), "conn=7",
		string(exec.DetailPoolRefused)} {
		if !strings.Contains(detail, want) {
			t.Errorf("event detail = %q, want it to carry %q", detail, want)
		}
	}
	for _, tok := range []string{"billing-prod", "autodb_rw", "db7.internal", "6432"} {
		if strings.Contains(strings.ToLower(detail), strings.ToLower(tok)) {
			t.Errorf("event detail carries %q from the raw cause: %q", tok, detail)
		}
	}
}
