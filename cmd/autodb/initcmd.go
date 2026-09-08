package main

// `autodb --init` — the first-run ceremony in one command.
//
// Standing a front door up used to need a detour: create the first
// administrator through a frontend, log in, then find the keyslot pane to cut
// the unattended-unlock slot. An installer could do neither, so every scripted
// bring-up stopped short of a daemon that survives its own reboot, and the
// operator finished the job by hand in a TUI. This is the same ceremony with
// one entry point.
//
// WHY IT CAN DO WHAT AN INSTALLER CANNOT. Enrolling the service keyslot is
// admin-only AND possible only while the store is unlocked, and the second half
// is structural rather than policy: EnrollServiceKeyslot wraps the master key,
// so it must be running in a process that HOLDS the key. Bootstrap generates
// that key and adopts it, and Login unwraps it from the caller's own slot --
// either way the token this command holds and the unlocked store arrive
// together, in one process. A shell script cannot reach that state; this can,
// because it IS the process that authenticated.
//
// It takes the instance lease, so it refuses to run against a store a daemon is
// already serving rather than opening a second writer behind its back.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// initAuditIP is the address this ceremony audits under.
//
// A local process has no peer address, and the audit row must say something
// true rather than something empty. Loopback is what it is: the operation
// arrived from this machine. It is also what the allowlist admits by default,
// so an install that has narrowed [security] ip_allowlist to exclude loopback
// refuses --init for the same reason it would refuse the TUI on the same box --
// consistently, rather than because this command carved an exception.
const initAuditIP = "127.0.0.1"

// defaultAdminName is offered, never imposed. "root" is what an operator
// reaching for a first account types, and the prompt still accepts anything.
const defaultAdminName = "root"

// minPassphraseLen is a floor on the ceremony, not a policy for the system.
// The store's own rules are the store's business; this refuses only the input
// that is obviously an accident -- an empty line, or a few characters typed to
// get past a prompt -- because the passphrase chosen here wraps every secret
// the install will ever hold.
const minPassphraseLen = 8

type initOpts struct {
	// prompt reads a line, echoing it. Injected so cells drive the ceremony
	// without a terminal.
	prompt func(label, def string) (string, error)
	// secret reads a line WITHOUT echoing it.
	secret func(label string) (string, error)
}

// runInit performs the first-run ceremony: create or authenticate an
// administrator, then enrol the service keyslot if the config asked for one.
func runInit(ctx context.Context, out io.Writer, configPath string, o initOpts) error {
	if o.prompt == nil {
		o.prompt = ttyPrompt
	}
	if o.secret == nil {
		o.secret = ttySecret
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	store, err := meta.Open(ctx, cfg.Meta)
	if err != nil {
		return fmt.Errorf("meta store: %w", err)
	}
	defer store.Close()

	// The lease is the whole reason this is safe to run on a live host: if a
	// daemon is serving this store, we are told to stop it rather than
	// becoming a second writer against the same rows.
	lease, err := meta.AcquireLease(ctx, store, cfg.Meta)
	if err != nil {
		if errors.Is(err, meta.ErrLeaseHeld) {
			return fmt.Errorf("refusing to initialise: %w\n"+
				"       an autodb is already serving this meta store. Stop it first\n"+
				"       (systemctl stop autodb-frontdoor), then run --init again", err)
		}
		return fmt.Errorf("instance lease: %w", err)
	}
	defer func() { _ = lease.Release() }()

	svc, err := auth.New(store,
		auth.WithConfigAllowlist(cfg.Security.IPAllowlist),
		auth.WithServiceKeyfile(cfg.Security.ServiceKeyfile))
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	needs, err := svc.NeedsBootstrap(ctx)
	if err != nil {
		return fmt.Errorf("checking for existing users: %w", err)
	}

	var token string
	if needs {
		fmt.Fprintln(out, "No users exist yet. Creating the first administrator.")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "This passphrase wraps the master key that encrypts every connection")
		fmt.Fprintln(out, "secret in the store. It is not recoverable: nothing on disk can be used")
		fmt.Fprintln(out, "to reconstruct it, which is the point. Record it somewhere durable.")
		fmt.Fprintln(out, "")
		token, err = bootstrapAdmin(ctx, out, svc, o)
	} else {
		fmt.Fprintln(out, "This store already has users. Log in as an administrator to continue.")
		fmt.Fprintln(out, "")
		token, err = loginAdmin(ctx, svc, o)
	}
	if err != nil {
		return err
	}

	return enrolKeyslot(ctx, out, svc, cfg, token)
}

// bootstrapAdmin creates the first administrator and returns its token.
func bootstrapAdmin(ctx context.Context, out io.Writer, svc *auth.Service, o initOpts) (string, error) {
	name, err := o.prompt("administrator name", defaultAdminName)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(name) == "" {
		return "", errors.New("an administrator needs a name")
	}

	// Twice, because a mistyped passphrase here is unrecoverable: it wraps a
	// master key that exists nowhere else, so the store it protects would be
	// permanently unopenable rather than merely locked out.
	pass, err := o.secret("passphrase")
	if err != nil {
		return "", err
	}
	if len(pass) < minPassphraseLen {
		return "", fmt.Errorf("passphrase is %d characters; it must be at least %d", len(pass), minPassphraseLen)
	}
	again, err := o.secret("passphrase (again)")
	if err != nil {
		return "", err
	}
	if pass != again {
		return "", errors.New("the two passphrases differ; nothing was created")
	}

	token, ident, err := svc.Bootstrap(ctx, name, pass, initAuditIP)
	if err != nil {
		return "", fmt.Errorf("creating the first administrator: %w", err)
	}
	fmt.Fprintf(out, "  created administrator %q (role %s)\n", ident.Name(), ident.Role())
	return token, nil
}

// loginAdmin authenticates an existing administrator and returns its token.
func loginAdmin(ctx context.Context, svc *auth.Service, o initOpts) (string, error) {
	name, err := o.prompt("administrator name", defaultAdminName)
	if err != nil {
		return "", err
	}
	pass, err := o.secret("passphrase")
	if err != nil {
		return "", err
	}
	token, _, err := svc.Login(ctx, name, pass, initAuditIP)
	if err != nil {
		return "", fmt.Errorf("login: %w", err)
	}
	return token, nil
}

// enrolKeyslot cuts the unattended-unlock slot, or explains precisely why it
// did not.
//
// A missing service_keyfile is NOT an error. It is the ordinary state of an
// install that has not opted in, and failing here would turn a deliberate
// choice into a broken ceremony. It reports what to set instead.
func enrolKeyslot(ctx context.Context, out io.Writer, svc *auth.Service, cfg config.Config, token string) error {
	fmt.Fprintln(out, "")
	if strings.TrimSpace(cfg.Security.ServiceKeyfile) == "" {
		fmt.Fprintln(out, "Unattended unlock: NOT configured.")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  Without it a restart leaves the store locked and every front-door")
		fmt.Fprintln(out, "  client gets 57P03 until a human logs in by hand. To enable it, set")
		fmt.Fprintln(out, "  [security] service_keyfile to a path in its OWN directory -- not the")
		fmt.Fprintln(out, "  one holding the meta store, because the store and the key that opens")
		fmt.Fprintln(out, "  it are two halves of one envelope -- then run --init again.")
		return nil
	}

	switch err := svc.EnrollServiceKeyslot(ctx, token, initAuditIP); {
	case err == nil:
		// enrolled just now; the report below says so.
	case errors.Is(err, auth.ErrServiceKeyslotExists):
		// ALREADY DONE IS NOT A FAILURE, and this case is the difference
		// between a command an installer can re-run and one it cannot. The
		// slot is cut once per install; a second --init finding it present has
		// arrived at the state it was asked to produce. Erroring here made a
		// repeat run fail on a healthy install, which is how an idempotent
		// installer ends up reporting a problem that does not exist.
		//
		// Deliberately NOT re-cut: replacing a live slot would invalidate the
		// keyfile the running daemon unlocks with, turning a no-op into an
		// outage on the next restart.
		fmt.Fprintf(out, "Unattended unlock: already enabled (%s)\n", cfg.Security.ServiceKeyfile)
		return nil
	default:
		// Reported, not swallowed: the administrator exists either way, and an
		// operator who is told the slot failed can retry it. Silence here
		// would leave them believing a reboot is survivable when it is not.
		return fmt.Errorf("enrolling the service keyslot: %w\n"+
			"       the administrator was created; only the unattended unlock failed", err)
	}

	fmt.Fprintf(out, "Unattended unlock: ENABLED (%s)\n", cfg.Security.ServiceKeyfile)
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "  This install now opens its own store at start, so a restart no longer")
	fmt.Fprintln(out, "  locks anybody out. WHAT IT COSTS, stated plainly: at-rest protection")
	fmt.Fprintln(out, "  becomes filesystem permissions and host security rather than a")
	fmt.Fprintln(out, "  passphrase that exists nowhere on disk. Anyone who can read both the")
	fmt.Fprintln(out, "  keyfile and the meta store has every secret.")
	return nil
}

// ttyPrompt reads an echoed line from the terminal.
//
// It reads /dev/tty rather than stdin so the ceremony still works when the
// installer that invoked it arrived through a pipe.
func ttyPrompt(label, def string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("--init needs a terminal to prompt on: %w", err)
	}
	defer tty.Close()

	if def != "" {
		fmt.Fprintf(tty, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(tty, "%s: ", label)
	}
	var line string
	if _, err := fmt.Fscanln(tty, &line); err != nil {
		// An empty line is a valid answer when there is a default, and
		// Fscanln reports that as an error rather than an empty string.
		line = ""
	}
	if strings.TrimSpace(line) == "" {
		return def, nil
	}
	return strings.TrimSpace(line), nil
}

// ttySecret reads a line without echoing it.
//
// NEVER from a flag or an environment variable: an argv passphrase is visible
// in ps to every user on the box, and an environment one leaks into child
// processes and crash dumps. The terminal is the only input for this.
func ttySecret(label string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("--init needs a terminal to read a passphrase: %w", err)
	}
	defer tty.Close()

	fmt.Fprintf(tty, "%s: ", label)
	b, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(tty)
	if err != nil {
		return "", fmt.Errorf("reading passphrase: %w", err)
	}
	return string(b), nil
}
