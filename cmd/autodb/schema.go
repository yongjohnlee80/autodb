package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// THE SCHEMA SCRIPTS FROM THE COMMAND LINE — docs/ops/schema-scripts.md.
//
//	autodb --apply-migration-scripts [--dry-run]   apply what is pending, and exit
//	autodb --revert-migration-script N             undo the latest script, N
//
// Both open the store WITHOUT migrating (meta.OpenNoMigrate) and take the
// instance lease before anything changes, holding it until they exit: a
// store a daemon is serving is refused before a statement runs. Open would
// not do — it applies pending scripts before returning, so a revert through
// it would first reapply the script it was asked to undo.
//
// A daemon applies pending scripts at every start as well; the install and
// update scripts run --apply-migration-scripts first, so a script that fails
// stops the update BEFORE the running daemon is replaced.

type schemaOpts struct {
	dryRun bool
	revert int // the script number to revert; 0 to apply
}

// openLeased opens the configured store without migrating and holds its
// lease; release closes both.
func openLeased(ctx context.Context, configPath, verb string) (*meta.Store, func(), error) {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return nil, nil, err
	}
	store, err := meta.OpenNoMigrate(ctx, cfg.Meta)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: opening the meta store: %w", verb, err)
	}
	lease, err := meta.AcquireLease(ctx, store, cfg.Meta, meta.LeaseHolder{Role: "schema", Version: version})
	if err != nil {
		_ = store.Close()
		if errors.Is(err, meta.ErrLeaseHeld) {
			return nil, nil, fmt.Errorf("%s: a daemon is serving this meta store (%w). Stop it first — "+
				"changing the schema under a running daemon could remove what it uses", verb, err)
		}
		return nil, nil, fmt.Errorf("%s: taking the instance lease: %w", verb, err)
	}
	return store, func() { _ = lease.Release(); _ = store.Close() }, nil
}

// parseScriptNumber reads --revert-migration-script's N in BASE 10, so a
// number copied from a script's name — 000008 — is 8. (flag.Int reads a
// leading 0 as octal, which would make 000008 an error and 000010 eight.) A
// whole script name is accepted too.
func parseScriptNumber(s string) (int, error) {
	digits := s
	if i := strings.IndexByte(s, '_'); i > 0 {
		digits = s[:i]
	}
	n, err := strconv.Atoi(strings.TrimLeft(digits, "0"))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("--revert-migration-script %q is not a script number (000002, or its name)", s)
	}
	return n, nil
}

func runSchema(ctx context.Context, out io.Writer, configPath string, o schemaOpts) error {
	if o.revert != 0 {
		return runRevertScript(ctx, out, configPath, o.revert)
	}
	verb := "apply-migration-scripts"
	store, release, err := openLeased(ctx, configPath, verb)
	if err != nil {
		return err
	}
	defer release()

	var st meta.ScriptStatus
	if o.dryRun {
		st, err = meta.PendingScripts(ctx, store)
	} else {
		st, err = meta.ApplyScripts(ctx, store)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", verb, err)
	}
	for _, w := range st.Warnings {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
	switch {
	case len(st.Pending) == 0:
		fmt.Fprintf(out, "the schema is up to date (%d scripts applied)\n", len(st.Applied))
	case o.dryRun:
		fmt.Fprintf(out, "would apply %d script(s):\n", len(st.Pending))
	default:
		fmt.Fprintf(out, "applied %d script(s):\n", len(st.Pending))
	}
	if st.Backup != "" {
		fmt.Fprintf(out, "backed up the store first: %s\n", st.Backup)
	}
	for _, name := range st.Pending {
		fmt.Fprintf(out, "  %s\n", name)
	}
	if baselinePending(st) {
		// What 000001 does depends on the store, and the updater reads this
		// line to know whether the previous binary can still open it: only an
		// ADOPTION leaves the schema as it was.
		fmt.Fprintln(out, baselineEffect(st.LegacyBefore, o.dryRun))
	}
	return nil
}

func baselinePending(st meta.ScriptStatus) bool {
	for _, n := range st.Pending {
		if strings.HasPrefix(n, "000001_") {
			return true
		}
	}
	return false
}

// baselineEffect says what 000001 does to a store at legacy version v.
func baselineEffect(v int, dryRun bool) string {
	verb := map[bool]string{false: "", true: "would "}[dryRun]
	switch {
	case v == 0:
		return "000001 " + verb + map[bool]string{false: "created", true: "create"}[dryRun] + " the store"
	case v < 17:
		return fmt.Sprintf("000001 %sfirst %s the store from legacy v%d to v17: a schema change",
			verb, map[bool]string{false: "brought", true: "bring"}[dryRun], v)
	default:
		return "000001 " + verb + map[bool]string{false: "adopted", true: "adopt"}[dryRun] +
			" a store already at v17: no schema change"
	}
}

func runRevertScript(ctx context.Context, out io.Writer, configPath string, n int) error {
	verb := "revert-migration-script"
	store, release, err := openLeased(ctx, configPath, verb)
	if err != nil {
		return err
	}
	defer release()
	name, err := meta.RevertScript(ctx, store, n)
	if err != nil {
		return fmt.Errorf("%s: %w", verb, err)
	}
	fmt.Fprintf(out, "reverted %s\n", name)
	fmt.Fprintf(out, "This is the first step of a downgrade: install the release before this "+
		"script now. Starting THIS binary again reapplies it (docs/ops/schema-scripts.md).\n")
	return nil
}
