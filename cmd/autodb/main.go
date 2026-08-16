// Command autodb is the autodb binary: an RPC server, a standalone TUI, and
// (via the bundled Lua integration) the backend of autovim's dbase section.
//
// --serve runs the msgpack-RPC server (roadmap M5); --ui lands at M6.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/rpc"
	"github.com/yongjohnlee80/golib/logger"
)

// version and commit are stamped at build time via
// -ldflags "-X main.version=<tag> -X main.commit=<sha>".
var (
	version = "dev"
	commit  = "none"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	serve := flag.Bool("serve", false, "run the RPC server")
	ui := flag.Bool("ui", false, "run the standalone TUI (not yet implemented - roadmap M6)")
	configPath := flag.String("config", "", "config file path (default: the user config dir)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("autodb %s (%s)\n", version, commit)
		return
	}

	switch {
	case *serve:
		if err := runServe(*configPath); err != nil {
			fmt.Fprintf(os.Stderr, "autodb: %v\n", err)
			os.Exit(1)
		}
	case *ui:
		fmt.Fprintln(os.Stderr, "autodb: --ui is not implemented yet (roadmap M6)")
		os.Exit(1)
	default:
		flag.Usage()
		os.Exit(1)
	}
}

// runServe implements the shared-server lifecycle (ADR-0056 §3): bind the
// configured address; when it is already taken, probe the occupant — a
// compatible autodb means "already running" (exit 0, the FE contract);
// anything else is a loud error. Serves until SIGINT/SIGTERM, then drains.
func runServe(configPath string) error {
	// config.Load owns path resolution: an empty path resolves to the
	// default location, a missing file means defaults, and everything else
	// (unreadable file, bad TOML, unknown keys) fails loudly — no silent
	// fallback away from the operator's intended bind/allowlist/meta.
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	// JoinHostPort, not Sprintf: an IPv6 bind ("::1") needs brackets.
	addr := net.JoinHostPort(cfg.Server.Bind, fmt.Sprintf("%d", cfg.Server.Port))

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if !isAddrInUse(err) {
			return fmt.Errorf("bind %s: %w", addr, err)
		}
		probeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		occupant, perr := rpc.Probe(probeCtx, addr)
		if perr == nil {
			fmt.Printf("autodb: already running on %s (version %s)\n", addr, occupant)
			return nil
		}
		return fmt.Errorf("bind %s: address in use, occupant is not a compatible autodb: %v", addr, perr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := meta.Open(ctx, cfg.Meta)
	if err != nil {
		ln.Close()
		return fmt.Errorf("meta store: %w", err)
	}
	defer store.Close()

	svc, err := auth.New(store, auth.WithConfigAllowlist(cfg.Security.IPAllowlist))
	if err != nil {
		ln.Close()
		return fmt.Errorf("auth: %w", err)
	}
	eng := exec.New(store, svc)
	defer eng.Close()

	// The operational logger is NOT optional: the transport deliberately
	// withholds error detail from the wire (deny-before-disclose), so the
	// server-side log is the only place withheld core errors, frame
	// diagnostics, panics, and reply failures exist at all.
	oplog := logger.New(logger.WithWriter(os.Stderr), logger.WithContext("autodb"))
	srv := rpc.New(svc, eng, cfg.Server, version,
		rpc.WithListener(ln), rpc.WithLogger(oplog))
	fmt.Printf("autodb %s serving msgpack-RPC on %s\n", version, addr)
	return srv.Run(ctx)
}

func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}
