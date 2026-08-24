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
	"github.com/yongjohnlee80/autodb/webserver"
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
	webUI := flag.Bool("web-ui", false, "serve the TUI to a browser (requires a running --serve daemon)")
	port := flag.Int("port", defaultWebPort, "port for --web-ui, bound on 127.0.0.1 only")
	configPath := flag.String("config", "", "config file path (default: the user config dir)")
	// Frontends need to know WHERE to dial, and that answer must have one
	// owner. Reimplementing the socket-path rules in Lua would be a
	// second resolver that silently drifts from this one — so the binary
	// reports it instead ([[shared-resolver-single-source-of-truth]]).
	printEndpoint := flag.Bool("print-endpoint", false,
		"print the resolved endpoint as <network>\\t<address> and exit")
	flag.Parse()

	if err := checkFlags(*serve, *ui, *webUI, *printEndpoint, *port); err != nil {
		fmt.Fprintf(os.Stderr, "autodb: %v\n", err)
		flag.Usage()
		os.Exit(2)
	}

	if *showVersion {
		fmt.Printf("autodb %s (%s, built %s)\n", version, commit, buildDate)
		return
	}

	switch {
	case *printEndpoint:
		if err := runPrintEndpoint(*configPath); err != nil {
			fmt.Fprintf(os.Stderr, "autodb: %v\n", err)
			os.Exit(1)
		}
	case *serve:
		if err := runServe(*configPath); err != nil {
			fmt.Fprintf(os.Stderr, "autodb: %v\n", err)
			os.Exit(1)
		}
	case *ui:
		if err := runUI(*configPath); err != nil {
			fmt.Fprintf(os.Stderr, "autodb: %v\n", err)
			os.Exit(1)
		}
	case *webUI:
		if err := runWebUI(*configPath, *port); err != nil {
			fmt.Fprintf(os.Stderr, "autodb: %v\n", err)
			os.Exit(1)
		}
	default:
		flag.Usage()
		os.Exit(1)
	}
}

// defaultWebPort is --web-ui's loopback port. A default at all is a convenience;
// what matters is that it is a FIXED number rather than an ephemeral one, so a
// user can bookmark the URL and an SSH forward can name it.
const defaultWebPort = 7010

// checkFlags rejects flag combinations that cannot mean anything, before any of
// them is acted on (ADR-0061 §2.1).
//
// A flag that silently does nothing is a flag someone will believe did something,
// so `--port` outside `--web-ui` is a usage error rather than an ignored value.
// That check must detect PRESENCE, not value: comparing against the default
// cannot tell "not given" from "given the default", so `--port=7010 --ui` — a
// user explicitly passing a flag that will be ignored — would slip through
// (lector r1 #5 on ADR-0061). flag.CommandLine.Visit reports only what was
// actually set.
func checkFlags(serve, ui, webUI, printEndpoint bool, port int) error {
	portSet := false
	flag.CommandLine.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			portSet = true
		}
	})
	if portSet && !webUI {
		return errors.New("--port applies to --web-ui only")
	}
	// EXACTLY ONE mode. The dispatch switch tries printEndpoint, serve, ui, web-ui
	// in that order, so any pairing silently runs whichever comes first — and
	// --web-ui --print-endpoint printed the endpoint and never served the UI
	// (lector r3 must-fix 2). --print-endpoint is a dispatch mode and must be
	// counted like the others; --version is a query handled before the switch and
	// is deliberately left to short-circuit.
	modes := 0
	for _, on := range []bool{serve, ui, webUI, printEndpoint} {
		if on {
			modes++
		}
	}
	if modes > 1 {
		return errors.New("--serve, --ui, --web-ui, and --print-endpoint are " +
			"mutually exclusive; pass exactly one")
	}
	if webUI {
		if port <= 0 || port > 65535 {
			// Zero is rejected rather than treated as "pick one": an ephemeral
			// port is one the user cannot predict, bookmark, or forward.
			return fmt.Errorf("--port %d is out of range (1..65535)", port)
		}
	}
	return nil
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
	ep, err := cfg.Server.Endpoint()
	if err != nil {
		return err
	}
	addr := ep.Address

	ln, err := listen(ep)
	if err != nil {
		if !isAddrInUse(err) {
			return fmt.Errorf("bind %s: %w", addr, err)
		}
		probeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		occupant, perr := rpc.ProbeOn(probeCtx, ep.Network, addr)
		if perr == nil {
			fmt.Printf("autodb: already running on %s (version %s)\n", addr, occupant)
			return nil
		}
		return fmt.Errorf("bind %s: address in use, occupant is not a compatible autodb: %v", addr, perr)
	}
	if ep.IsLocal() {
		// The socket file IS the access control, so it is owner-only.
		// Without this the umask decides who may talk to a service that
		// holds every database credential.
		if cerr := os.Chmod(addr, 0o600); cerr != nil {
			ln.Close()
			return fmt.Errorf("chmod %s: %w", addr, cerr)
		}
		// Leaving the file behind would make the next launch look
		// occupied when nothing is listening.
		defer func() { _ = os.Remove(addr) }()
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
	notesRoot, nerr := cfg.NotesRoot()
	if nerr != nil {
		ln.Close()
		return fmt.Errorf("notes root: %w", nerr)
	}
	oplog := logger.New(logger.WithWriter(os.Stderr), logger.WithContext("autodb"))
	srv := rpc.New(svc, eng, cfg.Server, version,
		rpc.WithListener(ln), rpc.WithLogger(oplog), rpc.WithNotesDir(notesRoot))
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
	ep, err := cfg.Server.Endpoint()
	if err != nil {
		return err
	}
	addr := ep.Address

	notesRoot, err := cfg.NotesRoot()
	if err != nil {
		return err
	}
	notes, err := tuiapp.NewNoteStore(notesRoot)
	if err != nil {
		return err
	}

	spawn := func() (string, error) { return spawnServe(configPath) }
	session := tuiapp.NewSessionOn(ep.Network, addr, logger.Nop{}, spawn)
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

// runWebUI serves the TUI to a browser (ADR-0061). It reaches the daemon ONLY
// through the same RPC client seam --ui uses, and unlike --ui it never starts one:
// a missing daemon is a startup failure here, not something to fix by spawning.
func runWebUI(configPath string, port int) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	ep, err := cfg.Server.Endpoint()
	if err != nil {
		return err
	}

	// FAIL FAST, before binding a port or building anything: an operator who
	// forgot `autodb --serve` should learn it now and not from a browser tab that
	// loads and then does nothing.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	daemonVersion, err := webserver.Preflight(ctx, ep.Network, ep.Address)
	if err != nil {
		return err
	}

	notesRoot, err := cfg.NotesRoot()
	if err != nil {
		return err
	}

	gw, err := webserver.New(webserver.Config{
		Network:   ep.Network,
		Addr:      ep.Address,
		Port:      port,
		NotesRoot: notesRoot,
		// Note visibility (ADR-0064 §2.3). Default per-user; workspace mode is
		// opt-in, bound to one subject, and validated at config load.
		NotesMode:    cfg.WebNotesMode(),
		NotesSubject: cfg.Web.NotesSubject,
		// Operational log to stderr. --web-ui is a server an operator leaves
		// running, so a refused Origin, a failed login, and session lifecycle
		// have to be VISIBLE: without this the gateway falls back to
		// logger.Nop and discards every record it is wired to emit —
		// including the `ref=` correlation ids the browser shows the user,
		// which exist only to be looked up here.
		Log: logger.New(logger.WithWriter(os.Stderr), logger.WithContext("autodb")),
		About: tuiapp.AboutInfo{
			Version: version, Commit: commit, BuildDate: buildDate,
			Repo: repoURL, Author: author,
			NotesDir: notesRoot, MetaEngine: cfg.Meta.Engine,
			MetaPath:   metaPathFor(cfg),
			ConfigPath: configPathFor(configPath),
		},
	})
	if err != nil {
		return err
	}

	// The URL on stdout, once, before serving: this is a frontend a user has to
	// go and open, so the address is the only output that matters.
	fmt.Printf("autodb --web-ui: http://127.0.0.1:%d/ (daemon %s on %s)\n",
		port, daemonVersion, ep.Address)
	return gw.Serve(ctx)
}

// metaPathFor resolves what the About modal should say the meta store is, the way
// runUI does: this frontend reports the paths IT would use rather than asking the
// server, so the splash works before anyone logs in.
func metaPathFor(cfg config.Config) string {
	p := cfg.Meta.Path
	if cfg.Meta.Engine == "sqlite" && p == "" {
		if d, err := config.DefaultMetaPath(); err == nil {
			return d
		}
	}
	if cfg.Meta.Engine == "postgres" {
		return "(postgres DSN from config)"
	}
	return p
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

// listen binds the endpoint, clearing a stale unix socket first.
//
// A daemon killed with SIGKILL leaves its socket file behind, and bind
// then fails EADDRINUSE even though nothing is listening. Distinguishing
// the two cases is a dial: if something answers, the address really is
// in use and the caller's existing single-instance path handles it; if
// nothing does, the file is debris and removing it is safe.
//
// This is the one failure mode a unix socket has that a TCP port does
// not, so it is handled here rather than left to the operator.
func listen(ep config.Endpoint) (net.Listener, error) {
	if !ep.IsLocal() {
		return net.Listen(ep.Network, ep.Address)
	}
	ln, err := net.Listen(ep.Network, ep.Address)
	if err == nil || !isAddrInUse(err) {
		return ln, err
	}
	// Something owns the path. Is it alive?
	c, derr := net.DialTimeout(ep.Network, ep.Address, 500*time.Millisecond)
	if derr == nil {
		c.Close()
		return nil, err // genuinely in use; let the caller probe it
	}
	if rerr := os.Remove(ep.Address); rerr != nil {
		return nil, fmt.Errorf("stale socket %s: %w", ep.Address, rerr)
	}
	return net.Listen(ep.Network, ep.Address)
}

// runPrintEndpoint reports where this configuration says to meet, so a
// frontend in another language can dial the same place without
// reimplementing the resolution rules (which differ per platform and
// would drift the moment either side changed).
//
// Output is one tab-separated line — `<network>\t<address>` — because it
// is parsed by machines, and a stable two-field line is harder to get
// wrong than JSON that grows fields.
func runPrintEndpoint(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	ep, err := cfg.Server.Endpoint()
	if err != nil {
		return err
	}
	fmt.Printf("%s\t%s\n", ep.Network, ep.Address)
	return nil
}
