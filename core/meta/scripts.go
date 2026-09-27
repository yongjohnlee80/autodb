package meta

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/engine"
	"github.com/yongjohnlee80/autodb/sql/deployments"
)

// THE SCHEMA SCRIPTS — docs/ops/schema-scripts.md.
//
// The store's schema is the set of update scripts in schema_version, each
// recorded with its digest. 000001 is v17, the last version of the legacy
// in-code list; a store older than that is brought to v17 by the frozen
// legacy chain (migrations.go) and then adopts 000001, all in the one
// transaction the whole upgrade runs in.
//
// schema_migrations, the legacy ledger, stays TRUE FOR THE BINARIES BEFORE
// THIS ONE: they read only it, and one opening a store with no v17 row there
// would replay v1..v17 against tables that already exist. So recording 000001
// also records v17 there, on a store 000001 created as well as one it adopted,
// and nothing is ever written there beyond 17.

// legacyBaseline is the legacy version script 000001 is.
const legacyBaseline = 17

const ledgerDDL = `CREATE TABLE IF NOT EXISTS schema_version (
	script     TEXT PRIMARY KEY,
	sha256     TEXT NOT NULL,
	applied_at BIGINT NOT NULL)`

const legacyLedgerDDL = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version INTEGER PRIMARY KEY,
	applied_at BIGINT NOT NULL)`

// applied is the ledger: each applied update script's digest, by name.
func appliedScripts(ctx context.Context, ex migExec) (map[string]string, error) {
	rows, err := ex.QueryContext(ctx, `SELECT script, sha256 FROM schema_version`)
	if err != nil {
		return nil, fmt.Errorf("meta: reading schema_version: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var name, sum string
		if err := rows.Scan(&name, &sum); err != nil {
			return nil, err
		}
		out[name] = sum
	}
	return out, rows.Err()
}

// schemaPlan is what an upgrade would do, and what it found.
type schemaPlan struct {
	legacy   int                  // the legacy ledger's version
	applied  map[string]string    // schema_version, by name
	pending  []deployments.Script // update scripts not yet applied, in order
	warnings []string             // applied scripts whose digest changed
}

// planScripts creates both ledgers if absent, applies the downgrade guards,
// and works out what is pending — changing nothing else.
func planScripts(ctx context.Context, ex migExec, eng engine.Name) (schemaPlan, error) {
	var p schemaPlan
	for _, ddl := range []string{legacyLedgerDDL, ledgerDDL} {
		if _, err := ex.ExecContext(ctx, ddl); err != nil {
			return p, fmt.Errorf("meta: creating the schema ledgers: %w", err)
		}
	}
	cur, err := currentVersionOn(ctx, ex)
	if err != nil {
		return p, err
	}
	p.legacy = int(cur)
	if p.legacy > legacyBaseline {
		return p, fmt.Errorf("meta: store schema version %d is newer than this binary's %d — refusing to open (downgrade guard)", p.legacy, legacyBaseline)
	}
	if p.applied, err = appliedScripts(ctx, ex); err != nil {
		return p, err
	}
	updates, err := deployments.Updates(eng)
	if err != nil {
		return p, err
	}
	known := map[string]string{}
	for _, s := range updates {
		known[s.Name] = s.SHA256
		if sum, done := p.applied[s.Name]; !done {
			p.pending = append(p.pending, s)
		} else if sum != s.SHA256 {
			p.warnings = append(p.warnings, fmt.Sprintf(
				"schema script %s was applied with digest %s but this binary's copy is %s — it is not re-run; a released script must never change",
				s.Name, short(sum), short(s.SHA256)))
		}
	}
	for name := range p.applied {
		if _, ok := known[name]; !ok {
			return p, fmt.Errorf("meta: the store has schema script %s, which this binary does not — refusing to open (downgrade guard)", name)
		}
	}
	return p, nil
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}

// applyAll brings the store's schema up to date on ONE executor: the legacy
// chain where the store predates v17, then every pending script in order.
// The caller runs it inside one transaction, so a failure anywhere leaves the
// store as it was. It returns the plan's warnings.
func applyAll(ctx context.Context, ex migExec, d dao.Dialect, eng engine.Name) ([]string, error) {
	p, err := planScripts(ctx, ex, eng)
	if err != nil {
		return nil, err
	}
	for _, s := range p.pending {
		if s.Number == 1 && p.legacy > 0 && p.legacy < legacyBaseline {
			// A store the legacy chain began and did not finish: it, and only
			// it, can bring the store to v17 (docs/ops/schema-scripts.md).
			if err := applyLegacy(ctx, ex, d, eng, p.legacy, legacyBaseline); err != nil {
				return nil, err
			}
		}
		if err := runScript(ctx, ex, s); err != nil {
			return nil, err
		}
		if err := record(ctx, ex, d, s); err != nil {
			return nil, err
		}
		if s.Number == 1 {
			if err := recordLegacyBaseline(ctx, ex, d); err != nil {
				return nil, err
			}
		}
	}
	return p.warnings, nil
}

// runScript executes a script's statements, naming the script and the line a
// failure came from.
func runScript(ctx context.Context, ex migExec, s deployments.Script) error {
	stmts, err := s.Statements()
	if err != nil {
		return err
	}
	for _, st := range stmts {
		if _, err := ex.ExecContext(ctx, st.Text); err != nil {
			return fmt.Errorf("meta: schema script %s, line %d: %w", s.Name, st.Line, err)
		}
	}
	return nil
}

func record(ctx context.Context, ex migExec, d dao.Dialect, s deployments.Script) error {
	if _, err := ex.ExecContext(ctx,
		`INSERT INTO schema_version (script, sha256, applied_at) VALUES (`+
			d.Placeholder(1)+`, `+d.Placeholder(2)+`, `+d.Placeholder(3)+`)`,
		s.Name, s.SHA256, time.Now().Unix()); err != nil {
		return fmt.Errorf("meta: recording schema script %s: %w", s.Name, err)
	}
	return nil
}

// recordLegacyBaseline writes v17 to the legacy ledger when it is not there,
// so a binary from before the scripts opens this store (see the file comment).
func recordLegacyBaseline(ctx context.Context, ex migExec, d dao.Dialect) error {
	if _, err := ex.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) SELECT `+d.Placeholder(1)+`, `+d.Placeholder(2)+
			` WHERE NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = `+d.Placeholder(3)+`)`,
		legacyBaseline, time.Now().Unix(), legacyBaseline); err != nil {
		return fmt.Errorf("meta: recording v%d in schema_migrations: %w", legacyBaseline, err)
	}
	return nil
}

// ScriptStatus is what --apply-migration-scripts reports.
type ScriptStatus struct {
	Applied  []string // already in the ledger
	Pending  []string // would be, or were just, applied
	Warnings []string
	// LegacyBefore is the legacy version the store was at before the apply:
	// what 000001, when pending, does to it — 0 creates it, 1..16 upgrades it
	// through the legacy chain first, 17 adopts it with no schema change.
	LegacyBefore int
}

// PendingScripts reports the ledger and changes NOTHING: for
// --apply-migration-scripts --dry-run. Planning creates the ledgers when they
// are absent, so it runs in a transaction that is always rolled back.
func PendingScripts(ctx context.Context, s *Store) (ScriptStatus, error) {
	var st ScriptStatus
	err := schemaTx(ctx, s.conn, s.engine, func(ex migExec) error {
		p, err := planScripts(ctx, ex, s.engine)
		if err != nil {
			return err
		}
		st = statusOf(p)
		return errDryRun
	})
	if errors.Is(err, errDryRun) {
		err = nil
	}
	return st, err
}

// errDryRun rolls a dry run's transaction back.
var errDryRun = errors.New("meta: dry run")

func statusOf(p schemaPlan) ScriptStatus {
	st := ScriptStatus{Warnings: p.warnings, LegacyBefore: p.legacy}
	for name := range p.applied {
		st.Applied = append(st.Applied, name)
	}
	sortNames(st.Applied)
	for _, sc := range p.pending {
		st.Pending = append(st.Pending, sc.Name)
	}
	return st
}

func sortNames(names []string) {
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
}

// ErrNotLatest refuses a revert of a script that is not the latest applied.
var ErrNotLatest = errors.New("meta: only the latest applied schema script may be reverted")

// RevertScript undoes update script number n and removes it from the ledger
// (docs/ops/schema-scripts.md). It is the first step of a downgrade and never runs against a
// live daemon: the caller opened the store with OpenNoMigrate and holds the
// instance lease for as long as this runs. 000001 has no revert; only the
// latest applied script may be reverted, one per call.
func RevertScript(ctx context.Context, s *Store, n int) (string, error) {
	if n == 1 {
		return "", fmt.Errorf("meta: 000001 is the baseline and has no revert (docs/ops/schema-scripts.md)")
	}
	dir := s.engine
	rev, ok, err := deployments.RevertOf(dir, n)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("meta: no revert script numbered %06d", n)
	}
	var reverted string
	err = schemaTx(ctx, s.conn, s.engine, func(ex migExec) error {
		p, err := planScripts(ctx, ex, s.engine)
		if err != nil {
			return err
		}
		latest, name := 0, ""
		for applied := range p.applied {
			if num, err := strconv.Atoi(strings.SplitN(applied, "_", 2)[0]); err == nil && num > latest {
				latest, name = num, applied
			}
		}
		if latest != n {
			return fmt.Errorf("%w: the latest is %s, not %06d", ErrNotLatest, name, n)
		}
		if err := runScript(ctx, ex, rev); err != nil {
			return err
		}
		if _, err := ex.ExecContext(ctx, `DELETE FROM schema_version WHERE script = `+s.conn.Dialect().Placeholder(1), name); err != nil {
			return fmt.Errorf("meta: removing %s from the ledger: %w", name, err)
		}
		reverted = name
		return nil
	})
	return reverted, err
}

// ApplyScripts is --apply-migration-scripts: the upgrade a daemon's start runs,
// on a store opened with OpenNoMigrate whose instance lease the caller holds.
func ApplyScripts(ctx context.Context, s *Store) (ScriptStatus, error) {
	var st ScriptStatus
	err := schemaTx(ctx, s.conn, s.engine, func(ex migExec) error {
		p, err := planScripts(ctx, ex, s.engine)
		if err != nil {
			return err
		}
		st = statusOf(p)
		_, err = applyAll(ctx, ex, s.conn.Dialect(), s.engine)
		return err
	})
	return st, err
}

// SchemaWarnings are what the upgrade at Open found worth saying: applied
// scripts whose digest changed. The daemon prints them at start.
func (s *Store) SchemaWarnings() []string { return s.schemaWarnings }
