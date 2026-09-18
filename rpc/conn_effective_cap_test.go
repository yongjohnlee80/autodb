package rpc_test

// WHAT conn.list PUBLISHES IS WHAT BINDS, NOT WHAT WAS ASKED FOR.
//
// A connection row's pool_max_conns is a REQUEST. poolLimitsFor keeps the
// SMALLER of it and the engine's own ceiling, so a row asking for more than
// the install allows simply does not get it.
//
// Publishing the row value made the connection card advertise a number no
// caller would ever reach, under a heading promising "ceilings ... autodb
// holds you to". The card's own title is that no number pretends to be yours;
// a request rendered as a cap is exactly such a number.
//
// THE OVER-LIMIT CASE IS THE ONLY ONE THAT DISCRIMINATES. Where the request is
// below the ceiling the two readings agree, so a cell built on the ordinary
// case passes whichever value is published and proves nothing.
//
// IT GOES OVER THE WIRE rather than calling a helper, because the defect was
// never in the arithmetic. It was in which number the handler reached for, and
// a cell that computes the cap itself would have passed against the broken
// handler.

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/rpc"
)

const (
	capTestEngineCeiling = 8
	capTestRowRequest    = 100
)

func TestConnList_PublishesTheEffectiveCapNotTheRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	store, err := meta.Open(ctx, config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	svc, err := auth.New(store, auth.WithConfigAllowlist([]string{"127.0.0.1/32", "::1/128"}))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	rootTok, _, err := svc.Bootstrap(ctx, "root", "root-passphrase", "127.0.0.1")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// AN INSTALL THAT GRANTS 8.
	eng := exec.New(store, svc, exec.WithPoolLimits(capTestEngineCeiling, 0, 0))
	t.Cleanup(func() { _ = eng.Close() })

	dsn := fmt.Sprintf("file:capcell%d?mode=memory&cache=shared", time.Now().UnixNano())
	connID, err := eng.CreateConnection(ctx, rootTok, "greedy", "sqlite", dsn, "127.0.0.1")
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	// ...AND A CONNECTION THAT ASKS FOR 100. Written straight to the row,
	// because the request is stored rather than validated against the ceiling:
	// that ordering is deliberate, so lowering the install-wide ceiling binds
	// every connection that had asked for more with no rows to rewrite.
	if uerr := store.Connections.OnCtx(ctx).With(meta.ConnID, connID).
		Set(meta.ConnPoolMaxConns, int64(capTestRowRequest)).Update(); uerr != nil {
		t.Fatalf("setting the row's pool request: %v", uerr)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := rpc.New(svc, eng, config.Server{Bind: "127.0.0.1", Port: 0}, "test-version",
		rpc.WithListener(ln))
	runCtx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		<-errc
	})

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c := &client{t: t, conn: conn, br: bufio.NewReader(conn)}
	c.hello()

	errVal, result := c.call("conn.list", rootTok)
	if errVal != nil {
		t.Fatalf("conn.list: %#v", errVal)
	}
	rows := result.([]any)
	if len(rows) != 1 {
		t.Fatalf("conn.list returned %d rows, want 1", len(rows))
	}
	got := capTestInt(t, rows[0].(map[string]any)["pool_max_conns"])

	if got == capTestRowRequest {
		t.Fatalf("conn.list published the row's REQUEST (%d); the engine ceiling is %d, so no "+
			"caller will ever reach %d and the card advertises a bound that does not exist",
			got, capTestEngineCeiling, capTestRowRequest)
	}
	if got != capTestEngineCeiling {
		t.Fatalf("conn.list published %d, want the effective cap %d", got, capTestEngineCeiling)
	}
}

// capTestInt reads whatever integer shape the codec produced.
func capTestInt(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case int64:
		return n
	case uint64:
		return int64(n)
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		t.Fatalf("pool_max_conns came back as %T (%#v), not a number", v, v)
		return 0
	}
}
