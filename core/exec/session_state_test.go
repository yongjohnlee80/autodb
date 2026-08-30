package exec

import (
	"errors"
	"strings"
	"testing"
)

// ADR-0074 §5 — locality and the per-GUC allowlist are SEPARATE axes, so the
// tests are separate too. Collapsing them is exactly the coarseness the ADR
// calls out: "SET" as a verb decides nothing.

func TestParseSet_LocalityAndName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		sql   string
		local bool
		name  string
	}{
		{"SET LOCAL lock_timeout = '5s'", true, "lock_timeout"},
		{"set local lock_timeout to '5s'", true, "lock_timeout"},
		{"SET lock_timeout = '5s'", false, "lock_timeout"},
		{"SET SESSION lock_timeout = '5s'", false, "lock_timeout"},
		// The name is normalized; the value is the server's business.
		{"SET LOCAL STATEMENT_TIMEOUT = '1min'", true, "statement_timeout"},
		{`SET LOCAL "work_mem" = '64MB'`, true, "work_mem"},
	}
	for _, tc := range cases {
		got, err := parseSet(tc.sql)
		if err != nil {
			t.Errorf("parseSet(%q) = %v", tc.sql, err)
			continue
		}
		if got.Local != tc.local || got.Name != tc.name {
			t.Errorf("parseSet(%q) = {local:%v name:%q}, want {local:%v name:%q}",
				tc.sql, got.Local, got.Name, tc.local, tc.name)
		}
	}

	// An unparseable SET is refused, not admitted on the assumption that it
	// was probably fine.
	for _, sql := range []string{"SET", "SET LOCAL", "SELECT 1", "SET LOCAL = '5s'"} {
		if _, err := parseSet(sql); err == nil {
			t.Errorf("parseSet(%q) succeeded; an unreadable SET must be refused", sql)
		}
	}
}

// A grammar-changing GUC is refused in EVERY form, LOCAL included, and the
// refusal is checked before locality — telling someone to add LOCAL to a
// statement that would still be refused wastes their next attempt.
func TestAdmitSet_GrammarGUCsRefusedInEveryForm(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"standard_conforming_strings", "backslash_quote", "sql_mode", "search_path"} {
		for _, local := range []bool{true, false} {
			for _, txOpen := range []bool{true, false} {
				err := admitSet(setStatement{Local: local, Name: name}, txOpen)
				if !errors.Is(err, ErrSetGUCRefused) {
					t.Errorf("admitSet(%s local=%v tx=%v) = %v, want ErrSetGUCRefused",
						name, local, txOpen, err)
					continue
				}
				if errors.Is(err, ErrSetNotLocal) {
					t.Errorf("%s was refused for its locality; it must be refused for WHAT IT IS, "+
						"or the caller adds LOCAL and is refused again", name)
				}
			}
		}
	}
}

// Non-LOCAL SET is refused for every role in every profile, and the refusal
// says what would happen and what to write instead.
func TestAdmitSet_NonLocalIsRefusedWithTheDirectedMessage(t *testing.T) {
	t.Parallel()

	err := admitSet(setStatement{Local: false, Name: "lock_timeout"}, true)
	if !errors.Is(err, ErrSetNotLocal) {
		t.Fatalf("admitSet(non-local) = %v, want ErrSetNotLocal", err)
	}
	for _, want := range []string{
		"persists on the underlying pooled connection",
		"leak to other users",
		"HINT: use SET LOCAL lock_timeout",
		"reverts automatically at COMMIT/ROLLBACK",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q:\n%s", want, err)
		}
	}
}

func TestAdmitSet_AllowlistAndTransaction(t *testing.T) {
	t.Parallel()

	// Admissible: LOCAL, allowlisted, inside a transaction.
	if err := admitSet(setStatement{Local: true, Name: "lock_timeout"}, true); err != nil {
		t.Errorf("SET LOCAL lock_timeout inside a transaction = %v, want nil", err)
	}
	// Off the allowlist, even LOCAL and even in a transaction.
	err := admitSet(setStatement{Local: true, Name: "timezone"}, true)
	if !errors.Is(err, ErrSetGUCRefused) {
		t.Errorf("an un-allowlisted GUC = %v, want ErrSetGUCRefused", err)
	}
	// Allowlisted and LOCAL, but with no transaction: there is no boundary
	// for it to revert at.
	err = admitSet(setStatement{Local: true, Name: "lock_timeout"}, false)
	if !errors.Is(err, ErrSetOutsideTx) {
		t.Errorf("SET LOCAL outside a transaction = %v, want ErrSetOutsideTx", err)
	}
}

// LOCK is admissible only inside a transaction, where it auto-releases.
func TestAdmitLock_OnlyInsideATransaction(t *testing.T) {
	t.Parallel()

	if err := admitLock(true); err != nil {
		t.Errorf("LOCK inside a transaction = %v, want nil", err)
	}
	err := admitLock(false)
	if !errors.Is(err, ErrLockOutsideTx) {
		t.Fatalf("LOCK outside a transaction = %v, want ErrLockOutsideTx", err)
	}
	if !strings.Contains(err.Error(), "released immediately") {
		t.Errorf("refusal %q should say why holding it requires a transaction", err)
	}
}

// The engine's own belt is written in the admissible form, so the rule the
// gate enforces is the rule the engine follows.
func TestServerBelt_IsItselfAdmissible(t *testing.T) {
	t.Parallel()

	rec := &recordingTx{}
	if err := armServerBelt(t.Context(), rec, "postgres", defaultTxLimits()); err != nil {
		t.Fatal(err)
	}
	st, err := parseSet(rec.execs[0])
	if err != nil {
		t.Fatalf("the engine's own belt does not parse as a SET: %v", err)
	}
	if err := admitSet(st, true); err != nil {
		t.Errorf("the engine emits a SET its own gate refuses: %v\n%s", err, rec.execs[0])
	}
}
