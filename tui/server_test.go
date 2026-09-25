package tui_test

// The real daemon the host's tests talk to: a full autodb server on a loopback
// port, its own errors in the test's log.

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/rpc"
	"github.com/yongjohnlee80/golib/logger"
)

// testLogger forwards the daemon's log payloads into the test's own log, so a
// failure dump carries them.
type testLogger struct{ t *testing.T }

func (l testLogger) Log(sev logger.Severity, payload any) {
	// THE SET IS NAMED, not ordered. logger.Severity is a STRING: `sev <=
	// SeverityError` compiles and compares lexically, which would have kept
	// "Debug" and dropped "Warning" -- a filter that reads like a threshold and
	// is not one. Info and Debug are excluded because a served-request line per
	// keystroke would bury exactly what this is for.
	switch sev {
	case logger.SeverityWarning, logger.SeverityError, logger.SeverityCritical:
		l.t.Logf("daemon [%v] %v", sev, payload)
	}
}

// startRealServer starts a real RPC server for the UI to talk to.
//
// The variadic options exist so a cell can configure the SERVER-SIDE state a
// flow depends on -- a front door with a CA file, for instance. Without them a
// UI cell can only ever reach the branches that need no configuration, which
// is how a card body nobody had rendered acquired a line nobody had asserted.
func startRealServer(t *testing.T, opts ...rpc.Option) (addr string) {
	t.Helper()
	ctx := context.Background()
	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32", "::1/128"}))
	if err != nil {
		t.Fatal(err)
	}
	eng := exec.New(store, svc)
	t.Cleanup(func() { _ = eng.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// THE SERVER'S OWN ERRORS REACH THE TEST LOG.
	//
	// It ran with logger.Nop, so a handler failure reached the client as the
	// bare "internal error" that wireErr gives anything it cannot make public,
	// and the reason was discarded. On the 2026-09-09 VM43 run a connection
	// creation failed exactly that way -- `create demo: internal error` in the
	// status line and nothing anywhere else -- and the cause was not
	// recoverable from the ledger. A harness that cannot say why its own
	// daemon refused something is an instrument with no scale on it.
	//
	// The caller's own options come LAST so a cell can still override.
	srv := rpc.New(svc, eng, config.Server{Bind: "127.0.0.1", Port: 0}, "e2e",
		append([]rpc.Option{rpc.WithListener(ln), rpc.WithLogger(testLogger{t})}, opts...)...)
	runCtx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-errc; err != nil {
			t.Errorf("server: %v", err)
		}
	})
	for srv.Addr() == "" {
		time.Sleep(time.Millisecond)
	}
	return srv.Addr()
}
