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
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	coreexec "github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/rpc"
	tuiapp "github.com/yongjohnlee80/autodb/tui"
	"github.com/yongjohnlee80/golib/logger"
	tuicore "github.com/yongjohnlee80/golib/tui"
	tuiterm "github.com/yongjohnlee80/golib/tui/term"
)

// version and commit are stamped at build time via
// -ldflags "-X main.version=<tag> -X main.commit=<sha>".
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

// repoURL and author are shown in the TUI's About modal.
const (
	repoURL = "https://github.com/yongjohnlee80/autodb"
	author  = "Yong Sung John Lee"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	serve := flag.Bool("serve", false, "run the RPC server")
	ui := flag.Bool("ui", false, "run the standalone TUI")
	configPath := flag.String("config", "", "config file path (default: the user config dir)")
	// The PATH is public and may appear in the process table; the file's
	// CONTENTS are the secret, and the server reads-and-unlinks them at
	// startup (ADR-0058 §3.2.1).
	nonceFile := flag.String("launch-nonce-file", "",
		"path to a one-time launch nonce; the server reads it once, deletes it, and echoes it in sys.hello")
	flag.Parse()

	if *showVersion {
		fmt.Printf("autodb %s (%s, built %s)\n", version, commit, buildDate)
		return
	}

	switch {
	case *serve:
		if err := runServe(*configPath, *nonceFile); err != nil {
			fmt.Fprintf(os.Stderr, "autodb: %v\n", err)
			os.Exit(1)
		}
	case *ui:
		if err := runUI(*configPath); err != nil {
			fmt.Fprintf(os.Stderr, "autodb: %v\n", err)
			os.Exit(1)
		}
	default:
		flag.Usage()
		os.Exit(1)
	}
}

// runServe implements the shared-server lifecycle (ADR-0056 §3): bind the
// configured address; when it is already taken, probe the occupant — a
// compatible autodb means "already running" (exit 0, the FE contract);
// anything else is a loud error. Serves until SIGINT/SIGTERM, then drains.
func runServe(configPath, nonceFile string) error {
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
	eng := coreexec.New(store, svc)
	defer eng.Close()

	// The operational logger is NOT optional: the transport deliberately
	// withholds error detail from the wire (deny-before-disclose), so the
	// server-side log is the only place withheld core errors, frame
	// diagnostics, panics, and reply failures exist at all.
	oplog := logger.New(logger.WithWriter(os.Stderr), logger.WithContext("autodb"))
	sopts := []rpc.Option{rpc.WithListener(ln), rpc.WithLogger(oplog)}
	if nonceFile != "" {
		// Fail to start rather than serve unprovable: a launcher that
		// asked for a nonce must not silently get a daemon that cannot
		// present one (ADR-0058 §3.2.1).
		nonce, nerr := rpc.ReadLaunchNonce(nonceFile)
		if nerr != nil {
			return nerr
		}
		sopts = append(sopts, rpc.WithLaunchNonce(nonce))
	}
	srv := rpc.New(svc, eng, cfg.Server, version, sopts...)
	fmt.Printf("autodb %s serving msgpack-RPC on %s\n", version, addr)
	return srv.Run(ctx)
}

func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

// runUI starts the standalone TUI (ADR-0057): it reaches the server ONLY
// through the RPC client seam, spawning `autodb --serve` when nothing
// answers. The spawned child is detached into its own session with stdio
// redirected to an owned log file — never the alternate-screen terminal —
// and deliberately survives TUI exit (the shared server, Objective 25).
func runUI(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(cfg.Server.Bind, fmt.Sprintf("%d", cfg.Server.Port))

	notesRoot := cfg.TUI.NotesDir
	if notesRoot == "" {
		base, derr := os.UserHomeDir()
		if derr != nil {
			return derr
		}
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			notesRoot = filepath.Join(xdg, "autodb", "notes")
		} else {
			notesRoot = filepath.Join(base, ".local", "share", "autodb", "notes")
		}
	}
	notes, err := tuiapp.NewNoteStore(notesRoot)
	if err != nil {
		return err
	}

	spawn := func() (string, error) { return spawnServe(configPath) }
	session := tuiapp.NewSession(addr, logger.Nop{}, spawn)
	defer session.Close()

	backend, err := tuiterm.Open()
	if err != nil {
		return fmt.Errorf("terminal: %w", err)
	}

	// The About modal reports what THIS frontend resolved — the paths it
	// would actually use — rather than asking the server, so the splash
	// works before anyone logs in.
	metaPath := cfg.Meta.Path
	if cfg.Meta.Engine == "sqlite" && metaPath == "" {
		if p, perr := config.DefaultMetaPath(); perr == nil {
			metaPath = p
		}
	}
	if cfg.Meta.Engine == "postgres" {
		metaPath = "(postgres DSN from config)"
	}
	activeConfig := configPathFor(configPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := tuiapp.New(session, notes, cancel, tuiapp.WithAbout(tuiapp.AboutInfo{
		Version: version, Commit: commit, BuildDate: buildDate,
		Repo: repoURL, Author: author,
		NotesDir: notesRoot, MetaEngine: cfg.Meta.Engine, MetaPath: metaPath,
		ConfigPath: activeConfig,
	}))
	app := tuicore.NewApp(model.Root(), tuicore.WithBackend(backend))
	return app.Run(ctx)
}

// configPathFor reports the config file actually in play: the explicit
// --config, else the default location when a file exists there, else ""
// (meaning the built-in defaults, which is worth saying out loud).
func configPathFor(explicit string) string {
	if explicit != "" {
		return explicit
	}
	p, err := config.DefaultPath()
	if err != nil {
		return ""
	}
	if _, statErr := os.Stat(p); statErr != nil {
		return ""
	}
	return p
}

// spawnServe starts a detached `autodb --serve` with stdio redirected to an
// owned append-only log (ADR-0057 §7 — a stderr line into the alternate
// screen would corrupt raw mode). It returns the log path so the session's
// bounded probe window can point the operator at the failure diagnostics.
func spawnServe(configPath string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	stateDir := os.Getenv("XDG_STATE_HOME")
	if stateDir == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", herr
		}
		stateDir = filepath.Join(home, ".local", "state")
	}
	logDir := filepath.Join(stateDir, "autodb")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return "", err
	}
	logPath := filepath.Join(logDir, "serve.log")
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return "", err
	}
	defer logFile.Close()

	args := []string{"--serve"}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	cmd := exec.Command(self, args...)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	// Reap the child if it exits (it normally outlives us; a startup
	// failure keeps refusing dials until the session's bounded probe
	// window expires with this log path in the error).
	go func() { _ = cmd.Wait() }()
	return logPath, nil
}
