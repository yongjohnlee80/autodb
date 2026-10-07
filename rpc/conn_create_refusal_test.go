package rpc_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/rpc"
	"github.com/yongjohnlee80/golib/logger"
)

// refusedDSN is the shape that reached Johno's TUI as "internal error"
// (2026-10-06): a postgres DSN whose sslrootcert names a file that is not
// there, so pgx refuses it at parse time. It carries a password, and the host
// is asserted separately, because the host is diagnosis on a host-local
// surface and topology anywhere else.
const (
	refusedPassword = "Pa55wordNotForTheWire"
	refusedHost     = "db7.internal"
	refusedCAPath   = "/nonexistent-autodb-cell/ca.pem"
)

var refusedDSN = fmt.Sprintf("postgres://johno:%s@%s:5432/tagus?sslmode=verify-full&sslrootcert=%s",
	refusedPassword, refusedHost, refusedCAPath)

// captureLog is a logger a cell can read back.
type captureLog struct {
	mu    sync.Mutex
	lines []string
}

func (c *captureLog) Log(_ logger.Severity, payload any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprint(payload))
}

func (c *captureLog) find(t *testing.T, sub string) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.lines {
		if strings.Contains(l, sub) {
			return l
		}
	}
	t.Fatalf("no log line carries %q; lines: %q", sub, c.lines)
	return ""
}

// ON A HOST-LOCAL SURFACE THE CALLER IS TOLD WHY. This is the defect: the
// cause was computed, logged, and withheld, and the TUI said "internal error".
// The password must still not cross; the CA path and the OS error are the
// diagnosis and must.
func TestConnCreate_HostLocalAnswersTheExactRefusal(t *testing.T) {
	t.Parallel()
	f := newFixture(t, rpc.WithDetailDisclosure(true))
	c := f.session(t)

	errVal, _ := c.call("conn.create", f.rootTok, "tagus", "postgres", refusedDSN)
	msg := mustErr(t, errVal, rpc.CodeConfigFailed)
	for _, want := range []string{"unable to read CA file", refusedCAPath, "no such file or directory", refusedHost} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal lost %q:\n  %s", want, msg)
		}
	}
	if strings.Contains(msg, refusedPassword) {
		t.Errorf("the refusal carries the password:\n  %s", msg)
	}
	// Nothing was stored: the fixture's own connection is the only one.
	if _, res := c.call("conn.list", f.rootTok); len(res.([]any)) != 1 {
		t.Errorf("conn.list = %#v, want only the fixture's connection", res)
	}
}

// A CLASSIFIER RULE IS TOLD TOO, not only a parse failure: these refusals are
// fixed text naming the option, and they reached the caller as "internal error"
// by the same route.
func TestConnCreate_HostLocalAnswersTheRuleThatRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t, rpc.WithDetailDisclosure(true))
	c := f.session(t)

	errVal, _ := c.call("conn.create", f.rootTok, "m", "mysql", "u:p@tcp(h:3306)/db?multiStatements=true")
	if msg := mustErr(t, errVal, rpc.CodeConfigFailed); !strings.Contains(msg, "must not enable multiStatements") {
		t.Errorf("the refusal does not name the rule:\n  %s", msg)
	}
}

// OFF-HOST, THE CAUSE-FREE SHAPE: the positive control above is what makes
// this assertion mean something. Equality, not absence, so a message that grew
// a field also fails.
func TestConnCreate_OffHostAnswersTheShapeOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t) // disclosure off: the safe default
	c := f.session(t)

	errVal, _ := c.call("conn.create", f.rootTok, "tagus", "postgres", refusedDSN)
	msg := mustErr(t, errVal, rpc.CodeConfigFailed)
	want := exec.NewConfigFailure(exec.ConfigStageDSN, 0, exec.DetailDSNRefused, nil).Error()
	if msg != want {
		t.Errorf("off-host message:\n got  %s\n want %s", msg, want)
	}
}

// THE DURABLE RECORD AND THE LOG. The audit row carries the closed-set facts
// and the caller's own name and engine, never the DSN; the log carries the
// scrubbed cause, because the transport stops logging a refusal once it is
// typed.
func TestConnCreate_RefusalIsAuditedAndLogged(t *testing.T) {
	t.Parallel()
	logs := &captureLog{}
	f := newFixture(t, rpc.WithDetailDisclosure(true), rpc.WithLogger(logs))
	c := f.session(t)

	errVal, _ := c.call("conn.create", f.rootTok, "tagus", "postgres", refusedDSN)
	mustErr(t, errVal, rpc.CodeConfigFailed)

	rows := f.audits(t, "connection_create_failed")
	if len(rows) != 1 {
		t.Fatalf("connection_create_failed rows = %d, want 1", len(rows))
	}
	d := rows[0].Detail
	for _, want := range []string{"tagus (postgres)", "stage=dsn", string(exec.DetailDSNRefused)} {
		if !strings.Contains(d, want) {
			t.Errorf("audit detail lost %q: %s", want, d)
		}
	}
	for _, bad := range []string{refusedPassword, refusedHost, refusedCAPath, "postgres://"} {
		if strings.Contains(d, bad) {
			t.Errorf("audit detail carries %q: %s", bad, d)
		}
	}

	line := logs.find(t, "connection refused before it was stored")
	for _, want := range []string{"unable to read CA file", refusedCAPath, "tagus"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line lost %q:\n  %s", want, line)
		}
	}
	if strings.Contains(line, refusedPassword) {
		t.Errorf("log line carries the password:\n  %s", line)
	}
}
