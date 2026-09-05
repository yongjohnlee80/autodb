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
	"strings"
	"syscall"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	coreexec "github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/autodb/frontdoor"
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
	// Meta-store migration (ADR-0079 §5). One-way by design: sqlite to
	// postgres. The reverse is refused by name rather than left to fail
	// obscurely.
	migrateToPG := flag.Bool("migrate-to-postgres", false,
		"copy a sqlite meta store into an empty postgres one (ONE-WAY) and exit")
	migrateFrom := flag.String("from", "", "--migrate-to-postgres: the sqlite meta-store path")
	migrateTo := flag.String("to", "", "--migrate-to-postgres: the destination postgres DSN")
	migrateDry := flag.Bool("dry-run", false,
		"--migrate-to-postgres: report what would be copied and write nothing")
	migrateInsecure := flag.Bool("allow-insecure-dsn", false,
		"--migrate-to-postgres: permit a destination DSN weaker than sslmode=verify-full")
	// TLS material for the front door. It exists because the alternative to a
	// one-command certificate is an operator turning TLS off, and ADR-0086 §10
	// is what that costs.
	createCert := flag.Bool("create-cert", false,
		"generate the front door's CA and server certificate, and exit")
	certDir := flag.String("cert-dir", "",
		"--create-cert: where to write the material (default: a tls/ directory beside the config)")
	var certHosts hostList
	flag.Var(&certHosts, "cert-hosts",
		"--create-cert: names and addresses clients will dial (default: frontdoor.tls_host_names)")
	certLeafOnly := flag.Bool("leaf-only", false,
		"--create-cert: reissue the server certificate from the existing CA (no redistribution)")
	certExportCA := flag.Bool("export-ca", false,
		"--create-cert: print ca.pem, the one file developers need, and exit")
	certForce := flag.Bool("force", false,
		"--create-cert: replace existing CA key material (invalidates every distributed ca.pem)")
	flag.Parse()

	if err := checkFlags(*serve, *ui, *webUI, *printEndpoint, *migrateToPG, *createCert, *port); err != nil {
		fmt.Fprintf(os.Stderr, "autodb: %v\n", err)
		flag.Usage()
		os.Exit(2)
	}

	if *showVersion {
		fmt.Printf("autodb %s (%s, built %s)\n", version, commit, buildDate)
		return
	}

	switch {
	case *createCert:
		if err := runCreateCert(os.Stdout, *configPath, createCertOpts{
			dir: *certDir, hosts: certHosts, leafOnly: *certLeafOnly,
			force: *certForce, exportCA: *certExportCA,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "autodb: %v\n", err)
			os.Exit(1)
		}
	case *migrateToPG:
		if err := runMigrateToPostgres(context.Background(), os.Stdout, migrateOpts{
			from: *migrateFrom, to: *migrateTo, dryRun: *migrateDry,
			allowInsecure: *migrateInsecure,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "autodb: %v\n", err)
			os.Exit(1)
		}
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
func checkFlags(serve, ui, webUI, printEndpoint, migrateToPG, createCert bool, port int) error {
	portSet := false
	flag.CommandLine.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			portSet = true
		}
	})
	if portSet && !webUI {
		return errors.New("--port applies to --web-ui only")
	}
	// The migration flags belong to their mode, for the same reason --port
	// belongs to --web-ui: a flag that is silently ignored outside its mode
	// reads as accepted.
	migFlags := []string{"from", "to", "dry-run", "allow-insecure-dsn"}
	var stray []string
	flag.CommandLine.Visit(func(f *flag.Flag) {
		for _, m := range migFlags {
			if f.Name == m && !migrateToPG {
				stray = append(stray, "--"+m)
			}
		}
	})
	if len(stray) > 0 {
		return fmt.Errorf("%s applies to --migrate-to-postgres only", strings.Join(stray, ", "))
	}
	// Same rule for the certificate flags, and it matters more here than for
	// --port: --force outside --create-cert reads as "do the thing anyway",
	// which is the last impression to leave on a flag that can invalidate
	// every ca.pem an organisation has handed out.
	certOnly := []string{"cert-dir", "cert-hosts", "leaf-only", "export-ca", "force"}
	stray = nil
	flag.CommandLine.Visit(func(f *flag.Flag) {
		for _, c := range certOnly {
			if f.Name == c && !createCert {
				stray = append(stray, "--"+c)
			}
		}
	})
	if len(stray) > 0 {
		return fmt.Errorf("%s applies to --create-cert only", strings.Join(stray, ", "))
	}
	// --export-ca reads an existing CA and --leaf-only reissues from one.
	// Together they are two different intentions, and the dispatch would
	// silently honour whichever is checked first.
	if createCert {
		leafOnly, exportCA := false, false
		flag.CommandLine.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "leaf-only":
				leafOnly = true
			case "export-ca":
				exportCA = true
			}
		})
		if leafOnly && exportCA {
			return errors.New("--leaf-only issues a new certificate and --export-ca only prints " +
				"the existing CA; pass one")
		}
	}
	// EXACTLY ONE mode. The dispatch switch tries printEndpoint, serve, ui, web-ui
	// in that order, so any pairing silently runs whichever comes first — and
	// --web-ui --print-endpoint printed the endpoint and never served the UI
	// (lector r3 must-fix 2). --print-endpoint is a dispatch mode and must be
	// counted like the others; --version is a query handled before the switch and
	// is deliberately left to short-circuit.
	modes := 0
	// --migrate-to-postgres is counted too, and it matters more than the
	// others: it is FIRST in the dispatch switch, so an unnoticed
	// `--migrate-to-postgres --serve` would migrate and never serve.
	for _, on := range []bool{serve, ui, webUI, printEndpoint, migrateToPG, createCert} {
		if on {
			modes++
		}
	}
	if modes > 1 {
		return errors.New("--serve, --ui, --web-ui, --print-endpoint, --migrate-to-postgres " +
			"and --create-cert are mutually exclusive; pass exactly one")
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
		// occupied when nothing is listening — but removing it
		// UNCONDITIONALLY is worse, and was a live bug.
		//
		// A daemon that exits AFTER a successor has bound the same path
		// deletes the SUCCESSOR'S socket file. The result is a listener that
		// `ss` reports as healthy on an orphaned inode while `ls` shows
		// nothing, and every client fails to dial with no error anywhere
		// explaining why. Reproduced by `pkill -f "autodb --serve"` followed
		// immediately by a start.
		//
		// The meta-store lease protects the STORE from two writers by holding
		// an flock on its inode; nothing protected the socket PATH. So this
		// removes the file only while it is still the one we created, compared
		// by identity (os.SameFile is dev+ino on unix) rather than by name —
		// the name is precisely what a successor reuses.
		//
		// A window remains between the check and the unlink: unix offers no
		// "remove if inode matches". It is orders of magnitude narrower than
		// the unconditional form and does not grow with how long the daemon
		// ran, which is what made the original reachable by an ordinary
		// restart.
		//
		// The identity itself needs a pin to mean anything — inode numbers are
		// recycled, and on ext4 immediately. See socketIdentity.
		// Go's *net.UnixListener unlinks the path BY NAME on Close unless told
		// otherwise, so without this the removal that actually happens in an
		// ordinary shutdown is the stdlib's name-based one and the identity
		// check below only ever takes its early return. The reviewer measured
		// exactly that: with unlink-on-close left at its default the file is
		// already gone by the time the defer runs.
		//
		// Turning it off routes EVERY removal through the identity check,
		// including the ordering neither of us could construct — a successor
		// binding before our own Close, reachable when a dial to a live but
		// saturated listener fails.
		if ul, ok := ln.(*net.UnixListener); ok {
			ul.SetUnlinkOnClose(false)
		}
		id, cerr := pinSocket(addr)
		if cerr != nil {
			ln.Close()
			return fmt.Errorf("stat %s: %w", addr, cerr)
		}
		defer removeIfStillOurs(addr, id)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := meta.Open(ctx, cfg.Meta)
	if err != nil {
		ln.Close()
		return fmt.Errorf("meta store: %w", err)
	}
	defer store.Close()

	// One engine per meta store, enforced before anything is served
	// (ADR-0074 §1). The bind above is a per-ENDPOINT singleton — it says
	// nothing about the store, so two endpoints could share one meta store
	// and each keep its own in-memory session registry and transaction
	// reservation, both believing they enforced a limit neither held.
	lease, err := meta.AcquireLease(ctx, store, cfg.Meta)
	if err != nil {
		ln.Close()
		if errors.Is(err, meta.ErrLeaseHeld) {
			// A refusal to serve, not a crash: the operator has another
			// autodb running against this store and needs to be told which
			// thing to stop, not handed a stack trace.
			return fmt.Errorf("refusing to serve: %w\n"+
				"       another autodb is already serving this meta store; stop it, or point this one at a different [meta] store", err)
		}
		return fmt.Errorf("instance lease: %w", err)
	}
	// Registered AFTER store.Close so defer's LIFO order releases the lease
	// FIRST. That ordering is load-bearing, not cosmetic: the postgres lease
	// pins a pool connection and pgxpool's Close waits for every acquired
	// connection, so closing the store first would hang shutdown.
	// TestInstanceLease_ReleaseBeforeStoreCloseDoesNotHang pins it.
	defer func() { _ = lease.Release() }()

	svc, err := auth.New(store, auth.WithConfigAllowlist(cfg.Security.IPAllowlist))
	if err != nil {
		ln.Close()
		return fmt.Errorf("auth: %w", err)
	}

	// Everything that makes the engine actually OBEY its configuration lives
	// in startEngine, as one function, so it can be exercised as one thing.
	// Splitting it out is not tidiness: the previous version wired all of
	// this inline, and deleting the janitor or the lease watcher from those
	// lines broke no test at all. A call site that no test reaches is a call
	// site that can be removed by accident.
	eng, serveCtx, leaseLost, stopServing, err := startEngine(ctx, cfg, store, svc, lease.Lost(),
		func(msg string) { fmt.Fprintf(os.Stderr, "autodb: %s\n", msg) })
	if err != nil {
		return err
	}
	// ORDER MATTERS, and defer is LIFO: eng.Close() is registered second so
	// it runs FIRST, which is backwards — the engine would tear down its
	// pools while the serve context was still live and work could still be
	// dispatched. Registering stopServing second means it cancels first, and
	// Close then stops and waits for engine-owned background work before
	// closing anything (PR #20 r1 SF).
	defer eng.Close()
	defer stopServing()

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

	// THE FRONT DOOR, started before the RPC surface and stopped after it.
	//
	// Before, because it validates its TLS material and its budgets and can
	// REFUSE — and a daemon that is going to refuse to start should do so
	// before it has told anyone it is serving. After for the stop, so a wire
	// session's teardown still has an engine to release into.
	fd, fdServe, ferr := startFrontDoor(serveCtx, cfg, eng, oplog)
	if ferr != nil {
		ln.Close()
		return ferr
	}
	if fd != nil && cfg.FrontDoor.CleartextDebug() {
		// THE OPERATOR'S RECORD (ADR-0086 R7). Unconditional and
		// undismissable, at EVERY start — not once, not behind a flag, and not
		// suppressible. The TUI's banner is the dismissible one; this is the
		// half that survives nobody looking at it.
		//
		// It names the BIND CLASS because "cleartext on loopback" and
		// "cleartext reachable off-host" are different risk states, and the
		// second must not be reachable by someone who read a banner describing
		// the first. A second config key gating the off-host case was
		// considered and dropped — Johno ruled the admin owns the deployment
		// decision — so this is where that decision is made visible instead.
		fmt.Fprint(os.Stderr, cleartextBanner(fd.Addr().String(), countDebugTokens(ctx, store, time.Now())))
	}

	var fdFailed <-chan error
	if fd != nil {
		defer fd.Close()
		fdFailed = superviseFrontDoor(serveCtx, fdServe, stopServing,
			func(msg string) { fmt.Fprintln(os.Stderr, msg) })
	}

	// The front-door reader closes over the listener so the card prints the
	// address that actually BOUND — cfg.FrontDoor.Bind can be ":0" or a name
	// resolving to several addresses, and Addr() is the only thing a client
	// can dial.
	//
	// Be precise about what it does NOT buy (reviewer's O1): whether a
	// listener exists is decided once, before this point, so `Listening` is
	// effectively a snapshot. The one transition it misses is the supervisor
	// stopping a door that failed while serving — during that drain a card can
	// still say listening. The daemon is on its way down in that case, so the
	// window is the drain rather than indefinite; it is named here rather than
	// implied away.
	frontDoorState := func() rpc.FrontDoorInfo {
		info := rpc.FrontDoorInfo{
			Enabled:    cfg.FrontDoor.Enabled,
			HostNames:  cfg.FrontDoor.TLSHostNames,
			RootCAFile: cfg.FrontDoor.TLSRootCAFile,
		}
		if fd != nil {
			info.Listening = true
			info.Addr = fd.Addr().String()
			// Read from the config that OPENED the listener, on the same
			// branch that reports it listening — so "listening" and "how it
			// is listening" cannot come from two different moments.
			info.Cleartext = cfg.FrontDoor.CleartextDebug()
		}
		return info
	}
	srv := rpc.New(svc, eng, cfg.Server, version,
		rpc.WithListener(ln), rpc.WithLogger(oplog), rpc.WithNotesDir(notesRoot),
		rpc.WithFrontDoor(frontDoorState))
	fmt.Printf("autodb %s serving msgpack-RPC on %s\n", version, addr)
	err = srv.Run(serveCtx)
	// A lease loss is reported as the failure it is. Without this the
	// shutdown is indistinguishable from a clean SIGTERM, and the one
	// condition an operator most needs to see would exit 0.
	select {
	case <-leaseLost:
		return fmt.Errorf("stopped serving: the instance lease on this meta store was lost")
	default:
	}
	// Same treatment for the front door: a surface that was configured and is
	// gone is a failure, not a clean exit, and reporting it as one is the only
	// way an operator learns without going looking.
	select {
	case fderr := <-fdFailed:
		return fmt.Errorf("stopped serving: the front door failed: %w", fderr)
	default:
	}
	return err
}

// startFrontDoor opens and serves the PostgreSQL wire listener, or returns nil
// when the surface is disabled.
//
// ONE PLACE ASKS WHETHER IT IS ENABLED, and it is frontdoor.EnabledFrom. A
// second site testing cfg.Enabled is how a surface ends up half-started —
// listening without its budgets, or budgeted without listening.
//
// The TLS material is proven BEFORE the bind, which is row 2.1b's requirement
// and the reason Open takes a *tls.Config rather than file paths: a front door
// that cannot prove who it is must not accept a connection in order to be
// asked. A failure here fails the daemon rather than degrading to a daemon
// without a front door, because an operator who configured one and got a
// running process without it would have no reason to look.
func startFrontDoor(ctx context.Context, cfg config.Config, eng *coreexec.Engine,
	oplog logger.Logger) (*frontdoor.Listener, <-chan error, error) {

	if !frontdoor.EnabledFrom(cfg.FrontDoor) {
		return nil, nil, nil
	}
	tlsCfg, err := frontdoor.LoadServerTLS(cfg.FrontDoor, time.Now())
	if err != nil {
		return nil, nil, fmt.Errorf("front door: %w", err)
	}
	l, err := frontdoor.Open(cfg.FrontDoor.Bind, tlsCfg, frontDoorOptions(cfg, eng, oplog))
	if err != nil {
		return nil, nil, fmt.Errorf("front door: %w", err)
	}
	errc := make(chan error, 1)
	go func() { errc <- l.Serve(ctx) }()
	fmt.Printf("autodb %s serving the PostgreSQL wire protocol on %s\n", version, cfg.FrontDoor.Bind)
	return l, errc, nil
}

// frontDoorOptions builds the listener's options.
//
// Extracted so the wiring is TESTABLE. It was not, and that is exactly how
// the front door shipped able to authenticate and unable to execute: Queries
// was never passed, every authenticated client got 0A000, and the whole
// frontdoor suite stayed green because each of its cells supplies the seam
// itself. The library was verified; the wiring was not.
func frontDoorOptions(cfg config.Config, eng *coreexec.Engine, oplog logger.Logger) frontdoor.Options {
	return frontdoor.Options{
		Authn:   eng,
		Cancels: eng,
		// The post-auth query path. Without it the listener authenticates a
		// client and then refuses every statement it sends.
		Queries:           eng,
		MaxConns:          cfg.FrontDoor.MaxConns,
		PreAuthMaxConns:   cfg.FrontDoor.PreAuthConns,
		AuthWorkers:       cfg.FrontDoor.AuthWorkers,
		AuthFailuresPerIP: cfg.FrontDoor.AuthFailuresPerIP,
		ControlLaneBytes:  cfg.FrontDoor.ControlLaneBytes,
		OnLog:             func(msg string) { logger.Notice(oplog, map[string]any{"frontdoor": msg}) },
		OnEvent: func(e frontdoor.Event) {
			// Emitted to the operational log, which is where every other
			// withheld detail on this daemon lives. The DURABLE audit row is
			// the auth slice's business, not the listener's — matrix §1.3
			// names the vocabulary, and turning these into meta-store writes
			// is a separate decision about what an anonymous peer can make
			// this process write.
			logger.Notice(oplog, map[string]any{
				"frontdoor": e.Kind, "reason": e.Reason, "peer": e.Peer, "detail": e.Detail,
			})
		},
	}
}

// superviseFrontDoor stops the daemon when the front door stops unexpectedly.
//
// A LOG LINE IS NOT SUPERVISION, which is what this was: Serve's error was
// written to the operational log and the RPC surface kept running, so a
// configured front door could vanish behind a daemon that looked healthy in
// every other respect. The one condition an operator most needs to see would
// have been a line in a file nobody greps until something else goes wrong.
//
// Same shape as watchLease, and for the same reason: these are the two ways
// this process can lose a thing it promised to provide while continuing to
// serve, and they should fail the same way.
//
// A nil error, or the context's own cancellation, is a CLEAN stop and fires
// nothing — otherwise every ordinary shutdown would report itself as a
// failure.
func superviseFrontDoor(ctx context.Context, serveErr <-chan error, stop func(), warn func(string)) <-chan error {
	fired := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
		case err := <-serveErr:
			if err == nil || errors.Is(err, context.Canceled) {
				return
			}
			fired <- err
			warn(fmt.Sprintf("autodb: the front door stopped serving (%v); refusing to keep "+
				"running without it", err))
			stop()
		}
	}()
	return fired
}

// startEngine builds the engine and starts everything that makes it obey its
// configuration: the janitor that enforces the timeouts, and the watcher that
// stops serving when the instance lease is lost.
//
// The three belong together because they fail together. An engine built with
// the right options but no janitor has bounds nothing enforces; a daemon that
// holds a lease it never reads is one that keeps serving after another engine
// has taken the store. Both were true here, and neither was visible, because
// the wiring was a handful of inline statements no test could reach.
//
// Returns the engine, the context the SERVER must run on, a channel that
// closes if the lease is lost, and the stop function.
func startEngine(
	ctx context.Context,
	cfg config.Config,
	store *meta.Store,
	svc *auth.Service,
	leaseLost <-chan struct{},
	onLog func(string),
) (*coreexec.Engine, context.Context, <-chan struct{}, func(), error) {
	// REFUSE TO SERVE a store whose logical ids are not unique, BEFORE
	// anything is started.
	//
	// After partitioning, PRIMARY KEY (id, started_at) no longer makes `id`
	// unique — the schema permits the same id in two months and accepts it
	// silently. The guard existed but nothing production ever called it
	// (lector's PR #32 r0 MF1), which made it documentation rather than a
	// guard: meta.Open accepted a store holding duplicate script_history ids
	// across partitions.
	//
	// It has to REFUSE rather than warn, because both things that depend on it
	// fail QUIETLY: a by-id read has no defined answer, and R4's
	// repairPendingHistory pages with OrderBy(id) + Gt(id, cursor), so a
	// non-monotonic id lets a row be skipped forever. A daemon that keeps
	// serving in that state produces wrong answers rather than errors.
	//
	// A no-op on sqlite, whose id is a plain primary key.
	if err := store.CheckLogicalIDUniqueness(ctx); err != nil {
		return nil, nil, nil, func() {}, fmt.Errorf("refusing to serve: %w", err)
	}

	serveCtx, stopServing := context.WithCancel(ctx)

	// The lease has to be CONSUMED, not merely acquired. Lost() closes when
	// the heartbeat can no longer confirm the lock: the backend was
	// terminated, the advisory lock dropped, the lease file vanished. With
	// nothing reading it, an engine that lost its lock kept serving — and a
	// second engine, finding the lock free, would take it. That is the
	// two-engines-one-store state the lease exists to prevent, reached
	// THROUGH the lease. So losing it is a shutdown, not a warning.
	lost := watchLease(serveCtx, leaseLost, stopServing, onLog)

	eng := coreexec.New(store, svc, execOptions(cfg, onLog)...)

	// And the janitor is STARTED. The timeout machinery is otherwise inert
	// in production: reapExpired had no caller outside tests, so an
	// abandoned transaction would hold locks on a live target for as long as
	// its client stayed connected — the exact failure the bounds exist for.
	// It stops with serveCtx.
	eng.StartJanitor(serveCtx, cfg.Exec.JanitorInterval.Duration())

	// And so is the outcome reconciler, for the same reason and with a
	// sharper edge: its STARTUP pass is what recovers the crash window. A
	// daemon that never called it would keep a complete, correct record of
	// every transaction whose fate it could not determine, and never once go
	// back to find out — which is the entire point of having written the
	// record down. It stops with serveCtx.
	// Partitions must stay AHEAD of the clock. Startup alone is not enough:
	// a daemon that stays up across a month boundary would find no partition
	// for the new month, and every audit write would land in DEFAULT — which
	// SUCCEEDS, so nothing fails and nobody notices until a retention drop
	// tries to detach a month whose rows are somewhere else (ADR-0079 §2).
	//
	// A no-op on sqlite, so this does not branch on the engine.
	if err := store.RollPartitions(serveCtx, time.Now()); err != nil {
		onLog(fmt.Sprintf("rolling partitions at startup: %v", err))
	}
	startPartitionRoll(serveCtx, store, onLog)

	eng.StartOutcomeReconciler(serveCtx, cfg.Exec.ReconcileInterval.Duration())

	// Retention is STARTED even though it is off by default, for the reason
	// this file already carries a warning about: machinery that is only ever
	// reachable from tests is machinery that silently never runs. It
	// returns immediately unless an operator has configured a retention
	// period (ADR-0079 §3).
	eng.StartOutcomeRetention(serveCtx, cfg.Exec.OutcomeRetentionInterval.Duration(),
		cfg.Exec.OutcomeRetention.Duration())

	return eng, serveCtx, lost, stopServing, nil
}

// execOptions maps the loaded configuration onto engine options.
//
// It exists as a named function so the mapping can be ASSERTED. Every setting
// here was previously parsed, validated, defaulted — and then dropped on the
// floor, because the call site passed only two of them. Nothing failed; an
// operator who set max_tx_duration simply got the built-in default and no
// indication that their value had gone nowhere. A configuration value that
// does not reach what it configures is worse than an absent one, since it
// reads as a decision that was made.
//
// Building the list separately from constructing the engine is what lets a
// test observe the mapping without a meta store, an auth service, or a
// listener.
func execOptions(cfg config.Config, onLog func(string)) []coreexec.Option {
	return []coreexec.Option{
		coreexec.WithHistory(cfg.History.Enabled),
		coreexec.WithMaxStatementBytes(cfg.Exec.MaxStatementBytes),
		coreexec.WithSessionLimits(cfg.Exec.MaxSessionsPerUser, cfg.Exec.MaxSessionsGlobal),
		coreexec.WithSessionIdleTimeout(cfg.Exec.SessionIdleTimeout.Duration()),
		coreexec.WithTxLimits(cfg.Exec.IdleInTxTimeout.Duration(), cfg.Exec.MaxTxDuration.Duration()),
		coreexec.WithDebugTxLimits(cfg.Exec.DebugIdleInTxTimeout.Duration(), cfg.Exec.MaxTxDurationCeiling.Duration()),
		coreexec.WithPoolLimits(cfg.Exec.PoolMaxConns,
			cfg.Exec.PoolMaxConnIdleTime.Duration(), cfg.Exec.PoolMaxConnLifetime.Duration()),
		coreexec.WithLogger(onLog),
		// The front door's two registry-scoped bounds. Both existed and
		// neither was ever set outside a test, so in a running daemon the
		// lease cap and the resident budget were zero — which is to say
		// absent. A guard nobody wires is documentation.
		coreexec.WithLeaseCap(cfg.FrontDoor.EffectiveMaxLeases(cfg.Exec.PoolMaxConns)),
		coreexec.WithResidentBudget(cfg.FrontDoor.EffectiveResidentBudget()),
	}
}

// watchLease stops the daemon when the instance lease is lost.
//
// The lease was acquired and then never consulted again: Lost() closes when
// the heartbeat can no longer confirm the lock — the backend was terminated,
// the advisory lock dropped, the lease file vanished — and with nothing
// reading it the engine kept serving without one. A second engine, finding
// the lock free, would then take it, which is exactly the two-engines-one-store
// state the lease exists to prevent, reached THROUGH the lease.
//
// So a lost lease is a shutdown, not a warning. The returned channel closes
// when that happens, so the caller can report the reason rather than exiting
// as if a clean signal had arrived.
func watchLease(ctx context.Context, lost <-chan struct{}, stop func(), warn func(string)) <-chan struct{} {
	fired := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-lost:
			close(fired)
			warn("autodb: the instance lease on this meta store was lost; refusing to keep serving " +
				"(another autodb may now hold it)")
			stop()
		}
	}()
	return fired
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
	// The terminal no longer builds a store at startup. It CANNOT: the personal
	// root is `<base>/u-<subject>`, and the subject is the daemon's canonical
	// identity, which does not exist until afterLogin. Constructing here is what
	// forced the terminal onto the ownerless base (ADR-0068 §1.3).
	notesFor := tuiapp.PersonalNotesIn(notesRoot)

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
	model := tuiapp.New(session, notesFor, cancel,
		tuiapp.WithLegacyNotes(notesRoot),
		tuiapp.WithAbout(tuiapp.AboutInfo{
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

// partitionRollInterval is how often the month-roll re-checks.
//
// Daily. The roll only ever creates the current and next month, so it is
// cheap and idempotent; the interval just has to be short enough that a
// month boundary cannot be crossed between two checks, and long enough that
// it is not noise. Anything under a month would do — a day leaves a wide
// margin for a daemon that is briefly paused or a clock that jumps.
const partitionRollInterval = 24 * time.Hour

// startPartitionRoll keeps the monthly partitions ahead of the clock.
func startPartitionRoll(ctx context.Context, store *meta.Store, onLog func(string)) {
	go func() {
		t := time.NewTicker(partitionRollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := store.RollPartitions(ctx, time.Now()); err != nil {
					onLog(fmt.Sprintf("rolling partitions: %v", err))
				}
			}
		}
	}()
}

// inodeHold is an open reference that keeps an inode ALLOCATED, so its number
// cannot be recycled under a successor. Its implementation is per-platform;
// socketIdentity deliberately has NO release method, so the only code that can
// drop a hold is withInodeHeld below.
type inodeHold interface{ release() }

// socketIdentity is what shutdown compares against, and it exists because
// os.SameFile IS NOT A SOUND IDENTITY FOR A PATH THAT CAN BE RECREATED.
//
// os.SameFile is dev+ino on unix, and INODE NUMBERS ARE REUSED. Measured on
// the two hosts this project builds on: unlink a file and recreate it at the
// same path and ext4 hands the number straight back — os.SameFile reports
// TRUE for a file it has never seen — while tmpfs reports false, because tmpfs
// allocates inode numbers from a monotonic counter. So the identity check
// shipped looking correct on a developer's /tmp and unsound on the machine it
// actually runs on. The filesystem, not the code, decides whether the bug
// appears; that is why the suite was green here and red on the test VM.
//
// AN OPEN REFERENCE closes it. While a descriptor holds the inode it stays
// allocated, so its number cannot be handed to a successor, so a successor's
// socket is GUARANTEED to compare unequal.
//
// THE FIRST VERSION OF THIS USED A HARD LINK AND CANCELLED ITSELF. The second
// name was derived from the socket path — sock + ".inode-pin" — so a successor
// binding the SAME path computed the SAME name and removed the predecessor's
// link on its way in, under a comment asserting that no live daemon could own
// it. That premise is true for a CRASHED predecessor and false for a
// SHUTTING-DOWN one, which is the case this whole path exists for: the
// guarantee could be revoked, at an arbitrary moment, by the very process it
// was protecting against. An open descriptor has no name to collide over and
// the kernel drops it on process exit, including a hard kill; a leftover file
// had neither property.
type socketIdentity struct {
	// hold keeps our inode allocated, or nil where the platform has no way to
	// take one.
	hold inodeHold
	// stat is the identity of record, captured while we hold the bind.
	stat os.FileInfo
}

// pinSocket takes the identity of the socket we just bound and pins its inode.
func pinSocket(sock string) (socketIdentity, error) {
	st, err := os.Stat(sock)
	if err != nil {
		return socketIdentity{}, err
	}
	h, herr := holdInode(sock)
	if herr != nil {
		// UNHELD. Fall back to the bare stat, which is exactly what shipped
		// before: sound where inode numbers are not recycled, unsound where
		// they are. Strictly no worse — and the alternative, declining to
		// remove our own socket at all, makes the next launch look occupied
		// while nothing is listening, which is the failure this whole block
		// exists to avoid.
		return socketIdentity{stat: st}, nil
	}
	return socketIdentity{hold: h, stat: st}, nil
}

// withInodeHeld runs fn with our inode reference held and releases it after.
//
// The release lives HERE and nowhere else on purpose. Dropping the hold before
// the comparison is the one ordering that matters, and no deterministic cell
// can observe it — losing that race IS the thing under test, so a cell would be
// flaky in exactly the way the bug is. It used to be a one-line edit inside
// removeIfStillOurs. Now it requires editing a different function whose entire
// body is the ordering, which a reader cannot mistake for an incidental change.
//
// Not a compile error; nothing within one package can be. But the wrong
// ordering is no longer something you can write by accident while looking at
// the comparison, and where an instrument cannot exist, removing the way to get
// it wrong is worth more than a cell that cannot see it.
func (id socketIdentity) withInodeHeld(fn func()) {
	if id.hold != nil {
		defer id.hold.release()
	}
	fn()
}

// removeIfStillOurs unlinks a socket path only while it is the same file we
// created, so a shutdown cannot delete a successor's socket.
//
// Split out from the defer so the decision is testable: the bug it fixes is
// invisible to any test that only checks "the file is gone afterwards".
func removeIfStillOurs(path string, id socketIdentity) {
	id.withInodeHeld(func() {
		if id.stat == nil {
			return
		}
		now, err := os.Stat(path)
		if err != nil {
			// Already gone, or not ours to look at. Either way, not ours to
			// remove.
			return
		}
		if !os.SameFile(id.stat, now) {
			// A successor bound this path while we were shutting down. Its
			// socket is a different file wearing our name, and unlinking it
			// would leave a running daemon that no client can reach.
			return
		}
		_ = os.Remove(path)
	})
}

// cleartextBanner is what an operator sees at every start of a front door
// serving without TLS.
//
// Split out and returning a string so its CONTENT is testable — a banner
// asserted only by "it printed something" is a banner whose worst version
// passes.
func cleartextBanner(addr string, debugTokens int) string {
	class := "REACHABLE OFF-HOST"
	detail := "Any host that can route to this address can read every token that crosses it."
	if h, _, err := net.SplitHostPort(addr); err == nil {
		if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
			class = "loopback only"
			detail = "Only processes on this machine can reach it — but anyone with a shell here can read the traffic."
		}
	}
	var b strings.Builder
	fmt.Fprintln(&b, "==============================================================================")
	fmt.Fprintln(&b, "  FRONT DOOR IS SERVING WITHOUT TLS.")
	fmt.Fprintln(&b, "")
	fmt.Fprintf(&b, "  listening on %s  (%s)\n", addr, class)
	fmt.Fprintf(&b, "  %s\n", detail)
	fmt.Fprintln(&b, "")
	fmt.Fprintln(&b, "  EVERY ACCESS TOKEN PRESENTED HERE CROSSES THE WIRE IN CLEARTEXT, and a")
	fmt.Fprintln(&b, "  token works from anywhere it is admitted until it is revoked. An")
	fmt.Fprintln(&b, "  intercepted one is a credential an attacker keeps.")
	fmt.Fprintln(&b, "")
	// The COUNT, so an operator enabling the mode sees what they are switching
	// on rather than discovering it. Zero is stated rather than omitted: "no
	// debug tokens exist" is the reassuring case and it should be visible.
	switch {
	case debugTokens < 0:
		fmt.Fprintln(&b, "  COULD NOT COUNT the cleartext debugging tokens \u2014 assume some exist.")
	case debugTokens == 0:
		fmt.Fprintln(&b, "  No cleartext debugging tokens exist. Nothing can connect until one is minted.")
	case debugTokens == 1:
		fmt.Fprintln(&b, "  1 cleartext debugging token exists and is usable RIGHT NOW.")
	default:
		fmt.Fprintf(&b, "  %d cleartext debugging tokens exist and are usable RIGHT NOW.\n", debugTokens)
	}
	fmt.Fprintln(&b, "")
	fmt.Fprintln(&b, "  This is a DEBUGGING mode. Turn it off by removing")
	fmt.Fprintln(&b, "  frontdoor.insecure_disable_tls from the config and restarting.")
	fmt.Fprintln(&b, "==============================================================================")
	return b.String()
}

// countDebugTokens reports how many cleartext debugging tokens are USABLE — the
// banner's claim is "usable RIGHT NOW", so it has to mean what the auth path
// means. VerifyPAT refuses a token on revocation AND on expiry
// (core/auth/pat.go), so a count that filtered only on revoked would report
// tokens nobody can present.
//
// Expiry is filtered in Go rather than in the query because the row filter is
// equality-only; the row count here is a handful by construction (the mint gate
// requires an admin and a cleartext front door), so there is nothing to
// optimise.
//
// A failure counts as UNKNOWN rather than zero. Telling an operator "nothing can
// connect" because a query failed would be the most reassuring possible lie at
// the worst possible moment.
func countDebugTokens(ctx context.Context, store *meta.Store, now time.Time) int {
	rows, err := store.PATs.OnCtx(ctx).
		With(meta.PATDebugCleartext, int64(1)).With(meta.PATRevoked, int64(0)).Select()
	if err != nil {
		return -1
	}
	n := 0
	for _, r := range rows {
		if now.Unix() < r.ExpiresAt {
			n++
		}
	}
	return n
}
