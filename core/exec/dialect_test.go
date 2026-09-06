package exec

import (
	"context"
	"errors"
	"testing"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/engine"
)

// THE NEGATIVE CELL, which is the one that matters for a capability.
//
// A capability probe is trivially satisfiable in the direction everyone tests:
// postgres has a belt, the belt gets armed, green. The direction that decides
// whether the interface is worth having is the ABSENT one — a target without
// the capability must take the no-op branch and the caller must carry on. An
// interface tested only where it is present is an interface nobody has proven
// is optional.
//
// Written per engine so a third engine arriving without a belt is a decision
// someone makes here rather than a default nobody notices.
func TestEveryEngineWithoutABeltTakesTheAbsentBranch(t *testing.T) {
	for _, n := range engine.All() {
		_, has := dialectFor(n).(StatementTimeoutBelt)
		if has != n.HasServerStatementTimeout() {
			t.Errorf("engine %s: the dialect %s a StatementTimeoutBelt but the "+
				"capability table says %v. The interface and the predicate "+
				"describe the same fact and must not drift — the predicate is "+
				"what a reader consults, the interface is what runs.",
				n, map[bool]string{true: "implements", false: "does not implement"}[has],
				n.HasServerStatementTimeout())
		}
		if has {
			continue
		}
		// AND THE ABSENT BRANCH MUST BE REACHED WITHOUT A CONNECTION. A nil tx
		// is the strongest available witness that nothing was executed: if the
		// probe ever stopped short-circuiting, this panics rather than quietly
		// passing on a recording fake that nobody inspected.
		if err := armServerBelt(context.Background(), nil, n, defaultTxLimits()); err != nil {
			t.Errorf("engine %s has no belt, so arming one must be a no-op, and "+
				"it returned %v", n, err)
		}
	}
}

// The present branch must actually execute, on the transaction it is given.
//
// The companion to the cell above: together they say the probe DISCRIMINATES,
// which neither says alone. Without this one, a dialectFor that returned nil
// for everything would pass the absent cell for every engine.
func TestTheBeltIsArmedOnTheTransactionItIsGiven(t *testing.T) {
	armed := 0
	for _, n := range engine.All() {
		belt, ok := dialectFor(n).(StatementTimeoutBelt)
		if !ok {
			continue
		}
		rec := &recordingTx{}
		if err := belt.ArmIdleTransactionBelt(context.Background(), rec, 90); err != nil {
			t.Fatalf("engine %s: arming the belt: %v", n, err)
		}
		if len(rec.execs) != 1 {
			t.Fatalf("engine %s: the belt ran %d statement(s), want exactly one",
				n, len(rec.execs))
		}
		armed++
	}
	if armed == 0 {
		t.Fatal("no engine implements StatementTimeoutBelt, so the cell above " +
			"holds for every engine vacuously — which is what a dialectFor " +
			"returning nil for everything would look like")
	}
}

// The grammar capabilities and the capability table must agree.
//
// TWO ENCODINGS OF ONE FACT, kept deliberately and guarded rather than
// collapsed. The predicate table is what a READER consults — "does postgres
// verify per connection?" should not require reading type assertions — and the
// interfaces are what RUNS. The register's answer for that shape is guard:
// leave both, and add a cell that fails when they disagree.
//
// Note the INVERSION, which is the reason this cell is not a copy of the belt
// one: a dialect implements PerStatementGrammarVerifier exactly when the
// engine does NOT verify per connection. A cell that asserted equality would
// pass only by accident.
func TestGrammarCapabilitiesMatchTheCapabilityTable(t *testing.T) {
	checked := 0
	for _, n := range engine.All() {
		d := dialectFor(n)
		_, perStatement := d.(PerStatementGrammarVerifier)
		if perStatement == n.VerifiesGrammarPerConnection() {
			t.Errorf("engine %s: PerStatementGrammarVerifier present=%v and "+
				"VerifiesGrammarPerConnection=%v — these are INVERSES. A target "+
				"that verifies per connection must not also be re-verified per "+
				"statement, and one that cannot must be.",
				n, perStatement, n.VerifiesGrammarPerConnection())
		}
		checked++
	}
	if checked < 3 {
		t.Fatalf("checked %d engine(s); the walk is not reaching them", checked)
	}
}

// A target that cannot drift must not claim a verification it did not make.
//
// SQLite's grammar is fixed: there is no session setting to read. It therefore
// implements NEITHER verifier — and the distinction that matters is between
// "no check was needed" and "a check passed". A dialect implementing
// VerifySessionGrammar as `return nil` would report the second while doing the
// first, which is the no-op-implementation shape the capability pattern exists
// to refuse.
func TestAFixedGrammarImplementsNoVerifier(t *testing.T) {
	fixed := 0
	for _, n := range engine.All() {
		d := dialectFor(n)
		_, session := d.(SessionGrammarVerifier)
		_, perStatement := d.(PerStatementGrammarVerifier)
		if session || perStatement {
			continue
		}
		fixed++
	}
	if fixed == 0 {
		t.Fatal("no engine has a fixed grammar, so this cell asserts nothing — " +
			"sqlite is expected to be one, and its absence here means either " +
			"the dialect table changed or an empty verifier was added")
	}
}

// Both verifiers must run their check on the querier they are handed.
func TestTheGrammarVerifiersQueryTheSessionTheyAreGiven(t *testing.T) {
	ran := 0
	for _, n := range engine.All() {
		v, ok := dialectFor(n).(SessionGrammarVerifier)
		if !ok {
			continue
		}
		rec := &recordingQuerier{}
		// The error is expected: the fake returns no rows. What is asserted is
		// that a QUERY WAS ATTEMPTED — a verifier that checked nothing would
		// return nil here and record nothing.
		_ = v.VerifySessionGrammar(context.Background(), rec)
		if len(rec.queries) != 1 {
			t.Errorf("engine %s: the verifier ran %d quer(ies), want exactly one — "+
				"a verifier that reads no session setting is asserting the "+
				"session is safe without looking", n, len(rec.queries))
		}
		ran++
	}
	if ran == 0 {
		t.Fatal("no engine implements SessionGrammarVerifier; the cell above " +
			"then holds for every engine vacuously")
	}
}

// recordingQuerier records the statements a verifier runs and returns no rows.
type recordingQuerier struct{ queries []string }

func (r *recordingQuerier) QueryContext(_ context.Context, sql string, _ ...any) (dao.Rows, error) {
	r.queries = append(r.queries, sql)
	return nil, errors.New("recordingQuerier: no rows")
}

// Every mode in the incompatible list must actually be REFUSED.
//
// FOUND BY A MUTATION THAT SHOULD HAVE FAILED AND DID NOT. Moving this list
// out of dsn.go, I retyped it from memory of the switch it came from and lost
// "ANSI" — a mode that implies ANSI_QUOTES, so losing it silently re-admits
// the quoting change the list exists to refuse. Restoring the entry was easy;
// the alarming part was that dropping it again as a deliberate mutation left
// the whole offline suite green. The vocabulary was security-relevant and
// nothing enumerated it.
//
// So this drives every listed mode through the verifier and requires a
// refusal, and drives a benign mode through and requires acceptance — without
// the second half, a verifier that refused everything would pass the first.
func TestEveryIncompatibleModeIsRefused(t *testing.T) {
	if len(lexerIncompatibleModes) == 0 {
		t.Fatal("the incompatible-mode list is empty; every assertion here is vacuous")
	}
	d := mysqlDialect{}
	for _, mode := range lexerIncompatibleModes {
		// As the server reports it: a comma-joined set, with the flag in it.
		err := d.VerifySessionGrammar(context.Background(),
			&fixedQuerier{value: "STRICT_TRANS_TABLES," + mode + ",NO_ENGINE_SUBSTITUTION"})
		if err == nil {
			t.Errorf("sql_mode containing %s was ACCEPTED. It changes how the "+
				"classifier must read a statement, so a statement classified "+
				"under one reading executes under another.", mode)
		}
	}
	// The negative half: an ordinary mode must pass, or the cell above is
	// satisfied by a verifier that refuses every session it is shown.
	if err := d.VerifySessionGrammar(context.Background(),
		&fixedQuerier{value: "STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION"}); err != nil {
		t.Errorf("an ordinary sql_mode was refused (%v); the cell above would "+
			"then pass for a verifier that refuses everything", err)
	}
}

// The postgres verifier refuses the one setting that changes where a statement
// ends, and accepts the setting that does not.
func TestStandardConformingStringsOffIsRefused(t *testing.T) {
	d := postgresDialect{}
	if err := d.VerifySessionGrammar(context.Background(), &fixedQuerier{value: "off"}); err == nil {
		t.Error("standard_conforming_strings=off was ACCEPTED; a backslash then " +
			"escapes inside a literal and the classifier's idea of where the " +
			"statement ends stops matching the server's")
	}
	if err := d.VerifySessionGrammar(context.Background(), &fixedQuerier{value: "on"}); err != nil {
		t.Errorf("standard_conforming_strings=on was refused (%v)", err)
	}
}

// fixedQuerier answers any query with one row carrying value.
//
// A FAKE THAT ANSWERS, unlike recordingQuerier which answers nothing. The two
// exist for different questions: recordingQuerier proves a verifier LOOKED,
// this one proves it JUDGED what it saw. A single fake doing both would make
// the first assertion depend on the second's fixture.
type fixedQuerier struct{ value string }

func (f *fixedQuerier) QueryContext(context.Context, string, ...any) (dao.Rows, error) {
	return &oneRow{value: f.value}, nil
}

type oneRow struct {
	value string
	done  bool
}

func (r *oneRow) Next() bool {
	if r.done {
		return false
	}
	r.done = true
	return true
}

func (r *oneRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("oneRow: want exactly one destination")
	}
	p, ok := dest[0].(*string)
	if !ok {
		return errors.New("oneRow: destination is not a *string")
	}
	*p = r.value
	return nil
}

func (r *oneRow) Close() error { return nil }
func (r *oneRow) Err() error   { return nil }

// The incompatible-mode list must contain the modes MySQL documents as
// changing how SQL is parsed.
//
// THE CELL ABOVE CANNOT CATCH A SHRINKING LIST, and the mutation matrix is how
// that surfaced: deleting "ANSI" and re-running left it green, because it
// iterates the variable it is checking. A self-referential enumeration tests
// that every listed thing behaves — never that the list is still complete.
//
// So this names the three INDEPENDENTLY, in the test, from what the modes mean
// rather than from what the variable holds. That is a deliberate second
// encoding: the register's `guard` answer, chosen because the alternative —
// deriving the list from the server at runtime — would make the classifier's
// safety depend on a query, and a target that answered wrongly would be
// trusted.
//
//	NO_BACKSLASH_ESCAPES — a backslash stops escaping, so a literal ends
//	                       somewhere else than the classifier thinks.
//	ANSI_QUOTES          — a double quote becomes an identifier quote rather
//	                       than a string quote.
//	ANSI                 — a compound mode that IMPLIES ANSI_QUOTES, which is
//	                       why listing the two above is not enough, and which
//	                       is the entry a move-by-retyping lost.
func TestTheIncompatibleModeListIsComplete(t *testing.T) {
	want := []string{"NO_BACKSLASH_ESCAPES", "ANSI_QUOTES", "ANSI"}
	have := map[string]bool{}
	for _, m := range lexerIncompatibleModes {
		have[m] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("sql_mode %s is not in lexerIncompatibleModes. It changes "+
				"how a statement must be read, so a session carrying it can "+
				"execute a statement the classifier read differently.", w)
		}
	}
	if len(lexerIncompatibleModes) < len(want) {
		t.Errorf("the list has %d entries and at least %d are required; an entry "+
			"was removed", len(lexerIncompatibleModes), len(want))
	}
}

// The two oracle capabilities and the capability table must agree.
//
// Kept as TWO checks rather than one loop over both, because the interfaces
// are deliberately separate: an engine could report an id without being able
// to answer about it later, and a cell that checked them together would
// silently accept that pairing as long as the two happened to move in step.
func TestTheOracleCapabilitiesMatchTheCapabilityTable(t *testing.T) {
	for _, n := range engine.All() {
		d := dialectFor(n)
		if _, ok := d.(TransactionIDReporter); ok != n.ReportsTransactionID() {
			t.Errorf("engine %s: TransactionIDReporter present=%v, table says %v",
				n, ok, n.ReportsTransactionID())
		}
		if _, ok := d.(CommitStatusOracle); ok != n.HasCommitStatusOracle() {
			t.Errorf("engine %s: CommitStatusOracle present=%v, table says %v",
				n, ok, n.HasCommitStatusOracle())
		}
	}
}

// A target with no id reporter captures no id, and does it without a
// connection.
//
// The nil transaction is the witness: a probe that stopped short-circuiting
// would panic here rather than pass quietly. And the captured id's ABSENCE is
// load-bearing downstream — the reconciler reads an empty xid as "nothing to
// ask about", so an engine silently returning a placeholder would produce a
// recovery record carrying an id nobody can resolve.
func TestAnEngineWithoutAnIDReporterCapturesNothing(t *testing.T) {
	absent := 0
	e := &Engine{}
	for _, n := range engine.All() {
		if _, ok := dialectFor(n).(TransactionIDReporter); ok {
			continue
		}
		absent++
		if got := e.captureTargetXID(context.Background(), nil, n); got != "" {
			t.Errorf("engine %s reports no transaction id but captureTargetXID "+
				"returned %q", n, got)
		}
	}
	if absent == 0 {
		t.Fatal("every engine implements TransactionIDReporter, so this cell " +
			"asserts nothing — which is what a dialectFor handing the same " +
			"dialect to everyone would look like")
	}
}

// The oracle runs its query on the querier it is handed, and asks about the
// transaction it is given.
//
// THE ARGUMENT IS THE POINT. A CommitStatus that ignored its xid would answer
// about whatever transaction the server last saw — the shape that makes a
// recovery decision about the wrong transaction, which is the one failure this
// whole path exists to prevent.
func TestTheOracleAsksAboutTheTransactionItIsGiven(t *testing.T) {
	asked := 0
	for _, n := range engine.All() {
		o, ok := dialectFor(n).(CommitStatusOracle)
		if !ok {
			continue
		}
		rec := &recordingArgsQuerier{}
		_, _ = o.CommitStatus(context.Background(), rec, "424242")
		if len(rec.queries) != 1 {
			t.Fatalf("engine %s: the oracle ran %d quer(ies), want one", n, len(rec.queries))
		}
		found := false
		for _, a := range rec.args {
			if s, ok := a.(string); ok && s == "424242" {
				found = true
			}
		}
		if !found {
			t.Errorf("engine %s: the oracle ran %q with args %v — the transaction "+
				"id it was given does not reach the query, so the answer is "+
				"about some other transaction",
				n, rec.queries[0], rec.args)
		}
		asked++
	}
	if asked == 0 {
		t.Fatal("no engine implements CommitStatusOracle; the cell above holds vacuously")
	}
}

// recordingArgsQuerier records the statement AND its arguments.
type recordingArgsQuerier struct {
	queries []string
	args    []any
}

func (r *recordingArgsQuerier) QueryContext(_ context.Context, sql string, args ...any) (dao.Rows, error) {
	r.queries = append(r.queries, sql)
	r.args = append(r.args, args...)
	return nil, errors.New("recordingArgsQuerier: no rows")
}
