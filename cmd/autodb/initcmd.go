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

// initAuditIP is the address this ceremony acts and audits under.
//
// A local process has no peer address, and the audit row must say something
// true rather than something empty. Loopback is what it is: the operation
// arrived from this machine.
//
// It is NOT self-evidently admitted, which a first version of this file
// asserted and a review disproved. Bootstrap takes an ip only for the session
// and audit rows and checks admission NOWHERE, so an install whose
// ip_allowlist excludes loopback would still have let --init create the
// permanent administrator -- refusing only on a later rerun, by which time
// there is nothing left to protect. admitBootstrap below is the gate that
// makes the intended behaviour real.
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
		if err := admitBootstrap(ctx, svc); err != nil {
			return err
		}
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

	if err := enrolKeyslot(ctx, out, svc, cfg, token); err != nil {
		return err
	}
	return createUsers(ctx, out, svc, o, token)
}

// createUsers offers to add further accounts, which is the other half of a
// first run.
//
// The ceremony used to stop at the administrator, so every developer account
// still had to be made in the TUI afterwards -- and on an install whose RPC
// endpoint is a unix socket, that meant root doing it for them. Creating them
// here needs nothing this command does not already hold: CreateUser wants an
// admin token and an unlocked store, and Bootstrap left both in this process.
//
// Empty name ends the loop, so an operator who wants only the administrator
// presses return once.
func createUsers(ctx context.Context, out io.Writer, svc *auth.Service, o initOpts, token string) error {
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Additional accounts. Each developer needs one to log in and mint their")
	fmt.Fprintln(out, "own PAT; a token is minted BY the person who will use it, bound to a")
	fmt.Fprintln(out, "connection you grant them. Leave the name empty to finish.")

	// BOUNDED, so this terminates whatever the prompt returns. An interactive
	// operator ends it with an empty line, but a caller that keeps answering
	// the same thing -- a script, a stuck pipe -- would otherwise spin here
	// forever, and a first-run ceremony that never returns is worse than one
	// that stops asking.
	const maxAccounts = 50
	for n := 1; n <= maxAccounts; n++ {
		fmt.Fprintln(out, "")
		name, err := o.prompt(fmt.Sprintf("  account %d name (empty to finish)", n), "")
		if err != nil {
			return err
		}
		if strings.TrimSpace(name) == "" {
			if n == 1 {
				fmt.Fprintln(out, "  none added; you can add them later from the TUI.")
			}
			return nil
		}
		if n == maxAccounts {
			fmt.Fprintf(out, "  stopping after %d accounts; add any more from the TUI.\n", maxAccounts)
		}

		role, err := o.prompt("    role (editor|reader|admin)", meta.RoleEditor)
		if err != nil {
			return err
		}
		switch role {
		case meta.RoleAdmin, meta.RoleEditor, meta.RoleReader:
		default:
			fmt.Fprintf(out, "    %q is not a role; skipped. Use editor, reader or admin.\n", role)
			continue
		}

		// Re-asked, not skipped. Silently dropping an account because of a
		// typo is how an operator ends up believing a developer exists.
		pass, err := readNewPassphrase(out, o, "    passphrase")
		if err != nil {
			fmt.Fprintf(out, "    %v -- account %q not created.\n", err, name)
			continue
		}

		// A failure here must not abandon the accounts already made, nor the
		// administrator and keyslot that came before it. Reported and the loop
		// continues.
		if _, cerr := svc.CreateUser(ctx, token, name, pass, role, initAuditIP); cerr != nil {
			fmt.Fprintf(out, "    could not create %q: %v\n", name, cerr)
			continue
		}
		fmt.Fprintf(out, "    created %q as %s\n", name, role)
	}
	return nil
}

// admitBootstrap refuses an address the allowlist does not cover, BEFORE the
// irreversible effect.
//
// The webserver learned this the hard way and its gateway carries the finding:
// a caller at a non-admitted address could reach Bootstrap, become the
// permanent first administrator, and only then be refused. Nothing undoes an
// account or restores one-shot bootstrap state, so the rightful operator would
// find the daemon already claimed. This command had the same hole.
//
// THE ORDERING IS INVERTED HERE relative to ordinary login, deliberately and
// for a reason that does not generalise: login checks the address after
// credentials so that an early refusal cannot reveal whether a name exists.
// Before bootstrap there are no accounts, so there is no such question to leak
// -- while there IS an irreversible effect to protect, which login does not
// have.
//
// The GLOBAL layer only, because there is no user whose rows could be
// consulted. That is the strictest of the two layers rather than a relaxation:
// an address no configured prefix covers cannot claim this daemon.
func admitBootstrap(ctx context.Context, svc *auth.Service) error {
	admitted, err := svc.IPAllowed(ctx, initAuditIP)
	if err != nil {
		return fmt.Errorf("checking address admission: %w", err)
	}
	if !admitted {
		return fmt.Errorf("refusing to bootstrap from %s: no [security] ip_allowlist prefix "+
			"covers it.\n"+
			"       Creating the first administrator is irreversible, so it is gated on the\n"+
			"       same allowlist every login is. Add a prefix that covers loopback (the\n"+
			"       shipped default is 127.0.0.1/32 and ::1/128) and run --init again",
			initAuditIP)
	}
	return nil
}

// readNewPassphrase asks twice and RE-ASKS on a mismatch or a short entry.
//
// It used to abort the whole ceremony on the first mistyped confirmation, and
// that is not a proportionate response to a typo: --init also enrols the
// keyslot and the playbook only starts the service when --init succeeds, so a
// single slip left an install with no administrator, no unattended unlock and
// a stopped daemon -- and the operator had to work out which of those three
// still needed doing.
//
// Bounded, because a prompt that can never be satisfied must still end: an
// input source that keeps returning different values would otherwise spin
// here. After the last attempt it gives up and says so, which is the same
// outcome as before but reached only when the operator genuinely cannot enter
// a matching pair.
func readNewPassphrase(out io.Writer, o initOpts, label string) (string, error) {
	const attempts = 3
	for i := 1; i <= attempts; i++ {
		pass, err := o.secret(label)
		if err != nil {
			return "", err
		}
		again, err := o.secret(label + " (again)")
		if err != nil {
			return "", err
		}

		switch {
		case pass != again:
			fmt.Fprintln(out, "    the two entries differ.")
		case len(pass) < minPassphraseLen:
			fmt.Fprintf(out, "    too short: %d characters, needs at least %d.\n",
				len(pass), minPassphraseLen)
		default:
			return pass, nil
		}

		if i < attempts {
			fmt.Fprintf(out, "    try again (%d of %d left).\n", attempts-i, attempts)
		}
	}
	return "", fmt.Errorf("no matching passphrase after %d attempts; nothing was created", attempts)
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
	// permanently unopenable rather than merely locked out. And re-asked
	// rather than fatal -- see readNewPassphrase.
	pass, err := readNewPassphrase(out, o, "passphrase")
	if err != nil {
		return "", err
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
		// ALREADY DONE IS NOT A FAILURE -- that is what makes this command
		// safe for an installer to call unconditionally. But the SLOT ROW IS
		// NOT EVIDENCE THE UNLOCK WORKS, which a first version of this file
		// treated it as. EnrollServiceKeyslot returns Exists from the database
		// row alone, before it ever opens service_keyfile, so a present row
		// beside a deleted, wrong-moded or mismatched keyfile reported a
		// reboot-safe install whose next start would sit locked. A review
		// found it.
		//
		// So prove the pair instead of trusting half of it, by doing exactly
		// what the next boot does. A success here is the same success the
		// daemon will have; a failure names which half is wrong.
		//
		// Still NOT re-cut on failure: replacing a live slot strands the
		// keyfile that opened the old one and leaves the operator holding a
		// file that looks exactly right. Recovery is theirs to choose.
		if verr := svc.UnlockWithServiceKeyslot(ctx); verr != nil {
			return fmt.Errorf("a service keyslot already exists, but it does not open: %w\n"+
				"       The administrator is fine; the UNATTENDED UNLOCK IS NOT -- the next\n"+
				"       restart will leave the store locked and front-door clients will get\n"+
				"       57P03 until someone logs in by hand.\n"+
				"       The slot row and %s disagree. Nothing was re-cut, because that would\n"+
				"       strand whichever half is still good. Restore the keyfile from backup,\n"+
				"       or remove the slot from a logged-in admin session (autodb --ui, SPC K)\n"+
				"       and run --init again", verr, cfg.Security.ServiceKeyfile)
		}
		fmt.Fprintf(out, "Unattended unlock: already enabled (%s)\n", cfg.Security.ServiceKeyfile)
		fmt.Fprintln(out, "  Verified by opening the slot, not by the presence of its row.")
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
// in ps to every user on the box, and an environment one is inherited by every
// child process. The terminal is the only input for this.
//
// It does NOT follow that the passphrase is unreachable once read. It becomes
// an ordinary Go string with the lifetime the runtime gives it, so a core dump
// of this process could still contain it. Removing the two channels an
// unprivileged neighbour can read is what this buys; anything stronger would
// need a deliberate dumpability design, and none is claimed here.
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
