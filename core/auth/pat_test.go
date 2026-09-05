package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/config"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// mustFrontDoorConn returns the id of a connection a PAT may legally be bound
// to, creating it (and the caller's grant on it) on first use.
//
// ADR-0086 §6 gates a mint on FOUR things: a grant, profile = session, engine =
// postgres, and a recorded target_db. A fixture missing any of them makes every
// mint observe THE GATE rather than whatever the test is about — so the
// fixture supplies all four, and the tests that exist to exercise the gates
// call CreatePAT directly with a deliberately broken connection instead.
func mustFrontDoorConn(t *testing.T, s *Service, userID int64) int64 {
	t.Helper()
	ctx := context.Background()
	const name = "fixture-frontdoor"
	if row, err := s.store.Connections.OnCtx(ctx).With(meta.ConnName, name).Get(); err == nil {
		return row.ID
	} else if !errors.Is(err, dao.ErrNoRows) {
		t.Fatalf("looking up the fixture connection: %v", err)
	}
	id, err := s.store.Connections.OnCtx(ctx).
		Set(meta.ConnName, name).Set(meta.ConnEngine, "postgres").
		Set(meta.ConnProfile, meta.ProfileSession).
		Set(meta.ConnTargetDB, "fixture_db").
		Set(meta.ConnDSNEnc, []byte{1}).Set(meta.ConnCreatedBy, userID).
		Set(meta.ConnCreatedAt, int64(1)).Set(meta.ConnUpdatedAt, int64(1)).Insert()
	if err != nil {
		t.Fatalf("creating the fixture connection: %v", err)
	}
	// The grant is inserted directly rather than through AddGrant, which would
	// need an admin token this helper does not take. Even an admin is DENIED a
	// read without a grant row (decide reads the grant for every non-Manage
	// action), so this is not optional scaffolding.
	if _, gerr := s.store.Grants.OnCtx(ctx).
		Set(meta.GrantUserID, userID).Set(meta.GrantConnID, id).
		Set(meta.GrantRole, meta.RoleReader).
		Set(meta.GrantGrantedBy, userID).Set(meta.GrantCreatedAt, int64(1)).
		Insert(); gerr != nil {
		t.Fatalf("granting the fixture connection: %v", gerr)
	}
	return id
}

// patConn is mustFrontDoorConn for a call site that has a token rather than a
// user id. Tests calling CreatePAT directly are exercising something OTHER
// than the binding gates — lifetime, the cap, allowed_ips — so they still need
// a connection those gates will accept, or they would observe the gate.
func patConn(t *testing.T, s *Service, tok string) int64 {
	t.Helper()
	ident, err := s.ValidateToken(context.Background(), tok)
	if err != nil {
		t.Fatalf("resolving the caller: %v", err)
	}
	return mustFrontDoorConn(t, s, ident.UserID())
}

func mustPAT(t *testing.T, s *Service, tok, name string, ips ...string) NewPAT {
	t.Helper()
	ctx := context.Background()
	ident, err := s.ValidateToken(ctx, tok)
	if err != nil {
		t.Fatalf("resolving the caller for CreatePAT(%q): %v", name, err)
	}
	p, err := s.CreatePAT(ctx, tok, name, mustFrontDoorConn(t, s, ident.UserID()), 0, ips)
	if err != nil {
		t.Fatalf("CreatePAT(%q): %v", name, err)
	}
	return p
}

// The credential's shape and what survives creation. The secret exists once,
// in the return value — the store keeps a selector and a digest, so a
// database backup is not a credential dump.
func TestPAT_CreateShowsTheSecretOnceAndStoresOnlyADigest(t *testing.T) {
	t.Parallel()
	s, store, _ := newSvc(t)
	tok, ident := mustBootstrap(t, s)

	p := mustPAT(t, s, tok, "laptop")
	if !strings.HasPrefix(p.Secret, PATPrefix) {
		t.Errorf("secret %q lacks the %q prefix; a recognisable marker is what lets a secret "+
			"scanner find one in a commit before anybody else does", p.Secret, PATPrefix)
	}
	selector, secret, ok := splitPAT(p.Secret)
	if !ok || selector == "" || secret == "" {
		t.Fatalf("secret %q does not split into selector and secret", p.Secret)
	}

	rows, err := store.PATs.OnCtx(context.Background()).With(meta.PATUserID, ident.UserID()).Select()
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %d err = %v", len(rows), err)
	}
	row := rows[0]
	if row.Selector != selector {
		t.Errorf("stored selector %q != issued %q", row.Selector, selector)
	}
	// THE property: the secret is not recoverable from the store.
	if strings.Contains(string(row.SecretHash), secret) {
		t.Error("the stored digest contains the secret")
	}
	for _, f := range []string{row.Selector, row.Name, row.AllowedIPs} {
		if f != "" && strings.Contains(f, secret) {
			t.Errorf("a stored field contains the secret: %q", f)
		}
	}
	if row.ExpiresAt <= row.CreatedAt {
		t.Error("the token does not expire after it was created")
	}
}

// Every credential failure is the SAME error. A caller cannot tell an unknown
// token from a wrong one, a revoked one, or an expired one.
func TestPAT_EveryFailureIsTheSameError(t *testing.T) {
	t.Parallel()
	s, _, ck := newSvc(t)
	tok, _ := mustBootstrap(t, s)
	ctx := context.Background()

	good := mustPAT(t, s, tok, "good")
	revoked := mustPAT(t, s, tok, "revoked")
	expiring := mustPAT(t, s, tok, "expiring")

	// Positive control: a valid token verifies. Without it, every refusal
	// below could be a function that refuses everything.
	if _, err := s.VerifyPAT(ctx, good.Secret); err != nil {
		t.Fatalf("a valid token was refused (%v); this test cannot observe a real refusal either", err)
	}
	if err := s.RevokePAT(ctx, tok, 0, "revoked"); err != nil {
		t.Fatalf("RevokePAT: %v", err)
	}

	sel, _, _ := splitPAT(good.Secret)
	for _, tc := range []struct {
		name  string
		token string
		after time.Duration
	}{
		{"a malformed credential", "not-a-token", 0},
		{"an unknown selector", PATPrefix + "aaaaaaaaaaaa.bbbbbbbbbbbb", 0},
		{"the right selector with the wrong secret", PATPrefix + sel + ".wrong-secret", 0},
		{"a revoked token", revoked.Secret, 0},
		{"an expired token", expiring.Secret, PATDefaultLifetime + time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.after > 0 {
				ck.t = ck.t.Add(tc.after)
				defer func() { ck.t = ck.t.Add(-tc.after) }()
			}
			_, err := s.VerifyPAT(ctx, tc.token)
			if !errors.Is(err, ErrPATInvalid) {
				t.Fatalf("%s = %v, want the single ErrPATInvalid — a caller who can tell these "+
					"apart can enumerate which tokens exist", tc.name, err)
			}
		})
	}
}

// Comparable work on every path, MEASURED. An early return on an unknown
// selector is a difference an attacker reads off the clock: they submit
// candidates and learn which selectors exist without ever holding a valid
// credential. Enumeration by timing is still enumeration.
func TestPAT_UnknownSelectorCostsTheSameAsAWrongSecret(t *testing.T) {
	s, _, _ := newSvc(t)
	tok, _ := mustBootstrap(t, s)
	ctx := context.Background()
	good := mustPAT(t, s, tok, "timing")
	sel, _, _ := splitPAT(good.Secret)

	median := func(token string) time.Duration {
		const rounds = 41
		var samples []time.Duration
		for i := 0; i < rounds; i++ {
			start := time.Now()
			_, _ = s.VerifyPAT(ctx, token)
			samples = append(samples, time.Since(start))
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		return samples[len(samples)/2]
	}

	unknown := median(PATPrefix + "zzzzzzzzzzzz.bbbbbbbbbbbb")
	wrong := median(PATPrefix + sel + ".wrong-secret")

	spread := unknown - wrong
	if spread < 0 {
		spread = -spread
	}
	_ = spread
	// Generous: this is a leak detector on a shared runner, not a benchmark.
	// It must fail on a branch that skips the hash entirely while tolerating
	// scheduler noise.
	// A coarse backstop only, and its limits are stated rather than implied:
	// the difference an early return makes is one SHA-256 and one 32-byte
	// compare, which a tolerance loose enough to survive a shared runner
	// cannot resolve. I verified that — an early-returning version PASSED
	// this comparison. It stays for gross regressions; the property itself is
	// asserted below, where it is decidable.
	const grossTolerance = 5 * time.Millisecond
	if spread > grossTolerance {
		t.Errorf("an unknown selector costs %s and a wrong secret costs %s (spread %s > %s)",
			unknown, wrong, spread, grossTolerance)
	}

	// THE PROPERTY, where it can actually be observed: both paths perform the
	// hash-and-compare. An early return on an unknown selector skips it, and
	// that is what a timing attacker measures — they submit candidates and
	// learn which selectors EXIST without ever holding a valid credential.
	before := s.PATCompareCount()
	_, _ = s.VerifyPAT(ctx, PATPrefix+"zzzzzzzzzzzz.bbbbbbbbbbbb")
	unknownWork := s.PATCompareCount() - before

	before = s.PATCompareCount()
	_, _ = s.VerifyPAT(ctx, PATPrefix+sel+".wrong-secret")
	wrongWork := s.PATCompareCount() - before

	if unknownWork != wrongWork {
		t.Fatalf("an unknown selector performed %d hash-compares and a wrong secret performed %d; "+
			"the shorter path is measurable from outside, and it answers 'does this selector "+
			"exist' to anyone willing to time it", unknownWork, wrongWork)
	}
	if unknownWork == 0 {
		t.Fatal("neither path performed a hash-compare; the counter is not wired to the work it " +
			"claims to count, so its agreement means nothing")
	}
}

// Caps, expiry bounds, and name uniqueness — the create-time contract.
func TestPAT_CreateContract(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	tok, _ := mustBootstrap(t, s)
	ctx := context.Background()

	mustPAT(t, s, tok, "first")
	if _, err := s.CreatePAT(ctx, tok, "first", patConn(t, s, tok), 0, nil); !errors.Is(err, ErrPATNameTaken) {
		t.Errorf("a duplicate name = %v, want ErrPATNameTaken — a person with several tokens "+
			"needs to know which is which when they come to revoke one", err)
	}
	if _, err := s.CreatePAT(ctx, tok, "", patConn(t, s, tok), 0, nil); err == nil {
		t.Error("an unnamed token was created")
	}
	if _, err := s.CreatePAT(ctx, tok, "too-long", patConn(t, s, tok), PATMaxLifetime+time.Hour, nil); !errors.Is(err, ErrPATBadExpiry) {
		t.Errorf("an over-long lifetime = %v, want ErrPATBadExpiry", err)
	}
	if _, err := s.CreatePAT(ctx, tok, "negative", patConn(t, s, tok), -time.Hour, nil); !errors.Is(err, ErrPATBadExpiry) {
		t.Errorf("a negative lifetime = %v, want ErrPATBadExpiry", err)
	}

	// The per-user cap. Filling it is the point: a cap nothing reaches is a
	// cap nothing tests.
	for i := 1; i < PATMaxPerUser; i++ {
		mustPAT(t, s, tok, "fill-"+string(rune('a'+i)))
	}
	_, err := s.CreatePAT(ctx, tok, "one-too-many", patConn(t, s, tok), 0, nil)
	if !errors.Is(err, ErrPATCapExceeded) {
		t.Fatalf("token %d = %v, want ErrPATCapExceeded", PATMaxPerUser+1, err)
	}
	// And revoking frees a slot — a cap counting REVOKED tokens would strand
	// a user who had rotated a few.
	if err := s.RevokePAT(ctx, tok, 0, "first"); err != nil {
		t.Fatalf("RevokePAT: %v", err)
	}
	if _, err := s.CreatePAT(ctx, tok, "after-revoke", patConn(t, s, tok), 0, nil); err != nil {
		t.Errorf("revoking freed no slot: %v", err)
	}
}

// allowed_ips must be a subset of the user's OWN rows; empty inherits.
func TestPAT_AllowedIPsMustBeASubset(t *testing.T) {
	t.Parallel()
	s, store, _ := newSvc(t)
	tok, ident := mustBootstrap(t, s)
	ctx := context.Background()

	if _, err := store.UserIPs.OnCtx(ctx).
		Set(meta.UIPUserID, ident.UserID()).
		Set(meta.UIPCIDR, "10.0.0.0/8").
		Set(meta.UIPLabel, "office").
		Set(meta.UIPCreatedAt, int64(1)).Insert(); err != nil {
		t.Fatalf("seeding a user row: %v", err)
	}

	// Inside the user's row: allowed, and canonicalized on write.
	p := mustPAT(t, s, tok, "narrow", "10.1.2.0/24")
	rows, _ := store.PATs.OnCtx(ctx).With(meta.PATName, "narrow").Select()
	if len(rows) != 1 || rows[0].AllowedIPs != "10.1.2.0/24" {
		t.Fatalf("stored allowed_ips = %q, want the canonical form", rows[0].AllowedIPs)
	}
	_ = p

	// WIDER than the user's row: refused. A token cannot widen where its
	// owner may connect from, or the per-user layer is advisory.
	if _, err := s.CreatePAT(ctx, tok, "wide", patConn(t, s, tok), 0, []string{"0.0.0.0/0"}); !errors.Is(err, ErrPATBadAllowedIPs) {
		t.Errorf("a token wider than its owner's rows = %v, want ErrPATBadAllowedIPs", err)
	}
	// Outside them entirely: refused.
	if _, err := s.CreatePAT(ctx, tok, "elsewhere", patConn(t, s, tok), 0, []string{"192.168.5.0/24"}); !errors.Is(err, ErrPATBadAllowedIPs) {
		t.Errorf("a token outside its owner's rows = %v, want ErrPATBadAllowedIPs", err)
	}
	// Not a CIDR at all.
	if _, err := s.CreatePAT(ctx, tok, "junk", patConn(t, s, tok), 0, []string{"not-a-cidr"}); !errors.Is(err, ErrPATBadAllowedIPs) {
		t.Errorf("junk allowed_ips = %v, want ErrPATBadAllowedIPs", err)
	}
	// Empty INHERITS rather than denying everything — an empty list that
	// denied would make the ordinary token useless.
	e := mustPAT(t, s, tok, "inherits")
	rows, _ = store.PATs.OnCtx(ctx).With(meta.PATName, "inherits").Select()
	if rows[0].AllowedIPs != "" {
		t.Errorf("empty allowed_ips stored as %q, want empty (inherit)", rows[0].AllowedIPs)
	}
	_ = e
}

// The subset helper must issue on the CALLER'S transaction, not the pool.
//
// My first version of this cell asserted only that the helper "works" from
// inside a transaction — and it does, either way, so the cell could not see
// the violation it existed for. Reverting the helper to OnCtx passed it.
//
// What actually distinguishes the two is VISIBILITY. A row inserted inside
// the caller's transaction and not yet committed is visible on that
// connection and invisible on the pool. So the helper is asked to validate
// against a row that only exists inside the transaction: on the caller's
// connection the subset check succeeds, and a pool read cannot see the row
// at all.
//
// That is the concrete harm behind the executor convention — an OnCtx inside
// a helper called with a resource held does not merely look untidy, it reads
// a different database state than the caller is working in.
func TestPAT_SubsetCheckIssuesOnTheCallersTransaction(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	_, ident := mustBootstrap(t, s)
	ctx := context.Background()

	err := s.inTx(ctx, func(tx *dao.Transaction) error {
		// Inserted INSIDE the transaction, and never committed by the time
		// the helper runs.
		if _, ierr := s.store.UserIPs.On(tx).
			Set(meta.UIPUserID, ident.UserID()).
			Set(meta.UIPCIDR, "10.0.0.0/8").
			Set(meta.UIPLabel, "uncommitted").
			Set(meta.UIPCreatedAt, int64(1)).Insert(); ierr != nil {
			return ierr
		}

		// On the caller's transaction: the row is visible, so the subset
		// check passes — and it must RETURN, which is itself the assertion.
		//
		// Bounded, because a helper that reaches for the pool here does not
		// fail, it BLOCKS: a single-connection store has one connection and
		// the caller is holding it. Unbounded, this cell hangs under exactly
		// the defect it exists to catch, and a test that hangs when the code
		// is wrong reports nothing. I had to kill two runs before this sank
		// in.
		type result struct {
			got string
			err error
		}
		primary := make(chan result, 1)
		go func() {
			g, e := s.canonicalAllowedIPs(ctx, tx, ident.UserID(), []string{"10.1.0.0/16"})
			primary <- result{g, e}
		}()
		select {
		case r := <-primary:
			if r.err != nil {
				t.Errorf("the helper could not see a row inserted in the caller's own "+
					"transaction (%v) — it is reading a different database state than "+
					"its caller", r.err)
			}
			if r.got != "10.1.0.0/16" {
				t.Errorf("in-transaction result = %q", r.got)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("the helper BLOCKED while its caller held the only connection — it is " +
				"issuing on the pool instead of the caller's transaction, which on a " +
				"single-connection store is a deadlock and on a pooled one is a read " +
				"outside the caller's snapshot")
		}

		// Positive control for the INSTRUMENT, and it must be BOUNDED.
		//
		// The pool read does not merely miss the row on this store — it
		// blocks, because a single-connection SQLite store has one
		// connection and the caller is holding it. That is the sharpest
		// possible demonstration of the hazard and the worst possible test
		// behaviour: an unbounded version hangs instead of failing, and a
		// test that hangs when the code is wrong reports nothing. My first
		// attempt did exactly that and had to be killed.
		//
		// So the pool read gets a short deadline and MUST NOT succeed.
		// Blocking and missing the row are both correct outcomes here; only
		// success would mean the two executors are indistinguishable, which
		// would make the assertion above meaningless.
		bounded, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, perr := s.canonicalAllowedIPs(bounded, nil, ident.UserID(), []string{"10.1.0.0/16"})
			done <- perr
		}()
		select {
		case perr := <-done:
			if perr == nil {
				t.Error("a POOL read saw a row still uncommitted in the caller's transaction; " +
					"the two executors are indistinguishable here, so the assertion above " +
					"proves nothing")
			}
		case <-bounded.Done():
			// Blocked on the connection the caller holds — the hazard,
			// observed.
		}
		return nil
	})
	if err != nil {
		t.Fatalf("inTx: %v", err)
	}
}

// MF1: the per-user cap holds under REAL concurrency, on PostgreSQL.
//
// One transaction is not mutual exclusion. Under READ COMMITTED, concurrent
// creates each read the same committed count, each find a free slot, and each
// insert — the transaction gives atomicity of the write, not exclusivity of
// the decision. I claimed one transaction closed this gap in the first
// version; it did not, and lector reproduced 19 active tokens against a cap
// of 16.
//
// SQLite masks it by serializing writers, which is why this cell is
// PostgreSQL-only: on sqlite the unsafe version passes.
//
// WHAT THIS CELL PROVES, precisely. It asserts the INVARIANT — the per-user
// cap holds under concurrency — and not which lock delivers it. Removing
// either lock alone leaves the other serializing this path, so only removing
// BOTH turns it red. That is the right target: the invariant is what callers
// depend on, and a cell pinned to one particular lock would fail the day
// someone legitimately restructures them.
//
// It does mean the GLOBAL cap has no concurrency cell of its own — reaching
// 512 active tokens to race it is not a reasonable test. The global cap
// relies on the guard row, which serializes every PAT creation install-wide.
// That is acceptable because creating a token is a human action taken rarely,
// but it is a real serialization point and is named here rather than left for
// someone to discover under load.
func TestPAT_CapHoldsUnderConcurrency(t *testing.T) {
	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; the cap race only reproduces on PostgreSQL")
	}
	ctx := context.Background()
	store, err := meta.Open(ctx, config.Meta{Engine: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := New(store, WithConfigAllowlist([]string{"127.0.0.1/32", "::1/128"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tok, _, err := s.Bootstrap(ctx, fmt.Sprintf("caprace%d", time.Now().UnixNano()),
		"cap-race-passphrase", testIP)
	if err != nil {
		t.Skipf("bootstrap unavailable on this store: %v", err)
	}

	// Fill to one below the cap, so exactly ONE more may be created.
	for i := 0; i < PATMaxPerUser-1; i++ {
		if _, cerr := s.CreatePAT(ctx, tok, fmt.Sprintf("fill-%02d", i), patConn(t, s, tok), 0, nil); cerr != nil {
			t.Fatalf("prefill %d: %v", i, cerr)
		}
	}

	widened := make(chan struct{})

	// Then race, with the window WIDENED deterministically.
	//
	// Eight goroutines starting together is not a reliable instrument: my
	// first version of this cell had no hook, and the mutation removing
	// serialization PASSED — the racers happened to serialize on their own.
	// A probabilistic cell that misses the defect is not evidence.
	//
	// The hook runs inside CreatePAT's check→insert window. With the locks
	// held a competitor blocks there, so exactly one wins; without them
	// every racer sails through the widened gap and the overrun is certain.
	s.SetTestAfterCapLock(func() {
		// Only the FIRST arrival waits; the rest proceed immediately, so a
		// serialized run cannot deadlock behind its own hook.
		select {
		case <-widened:
		default:
			close(widened)
			time.Sleep(300 * time.Millisecond)
		}
	})
	const racers = 8
	var wg sync.WaitGroup
	errs := make([]error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = s.CreatePAT(ctx, tok, fmt.Sprintf("race-%02d", i), patConn(t, s, tok), 0, nil)
		}(i)
	}
	close(start)
	wg.Wait()

	var won, refused int
	for i, e := range errs {
		switch {
		case e == nil:
			won++
		case errors.Is(e, ErrPATCapExceeded):
			refused++
		default:
			t.Errorf("racer %d failed unexpectedly: %v", i, e)
		}
	}

	ident, _ := s.ValidateToken(ctx, tok)
	active, cerr := store.PATs.OnCtx(ctx).With(meta.PATUserID, ident.UserID()).
		With(meta.PATRevoked, int64(0)).Count()
	if cerr != nil {
		t.Fatalf("counting: %v", cerr)
	}

	if active > PATMaxPerUser {
		t.Fatalf("%d active tokens against a cap of %d (%d racers won, %d were refused): the "+
			"count-then-insert decision was not serialized, so every racer saw the same free "+
			"slot and took it", active, PATMaxPerUser, won, refused)
	}
	if won != 1 {
		t.Errorf("%d racers won, want exactly 1 — the cap had one slot left", won)
	}
	if refused != racers-1 {
		t.Errorf("%d refused, want %d", refused, racers-1)
	}
}

// MF1: a disabled account's token stops working immediately.
//
// SetUserDisabled revokes SESSIONS, not tokens, so without a per-call owner
// check a credential sitting in a DSN outlived the account it belonged to —
// offboarding looked complete and was not.
func TestPAT_DisabledOwnersTokenStopsWorking(t *testing.T) {
	t.Parallel()
	s, store, _ := newSvc(t)
	rootTok, _ := mustBootstrap(t, s)
	ctx := context.Background()

	if _, err := s.CreateUser(ctx, rootTok, "bob", "bob-passphrase-long", "editor", testIP); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	bobTok, _, err := s.Login(ctx, "bob", "bob-passphrase-long", testIP)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	pat, err := s.CreatePAT(ctx, bobTok, "bobs-laptop", patConn(t, s, bobTok), 0, nil)
	if err != nil {
		t.Fatalf("CreatePAT: %v", err)
	}

	// Positive control: it works while the account is live. Without this the
	// assertion below would pass against a token that never worked at all.
	if _, verr := s.VerifyPAT(ctx, pat.Secret); verr != nil {
		t.Fatalf("a fresh token was refused (%v); this test cannot observe the disable either", verr)
	}

	bob, err := store.Users.OnCtx(ctx).With(meta.UserName, "bob").Get()
	if err != nil {
		t.Fatalf("reading bob: %v", err)
	}
	if err := s.SetUserDisabled(ctx, rootTok, bob.ID, true, testIP); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}

	// THE EFFECT: the credential is dead on the next call, with the same
	// uniform failure as any other bad token.
	if _, verr := s.VerifyPAT(ctx, pat.Secret); !errors.Is(verr, ErrPATInvalid) {
		t.Fatalf("a disabled account's token still verifies (%v) — a credential in a DSN "+
			"outlives the account it belongs to, and offboarding is incomplete", verr)
	}
}

// The owner check must not reintroduce the timing oracle it sits next to.
// It runs AFTER the match, so an unknown selector and a wrong secret — the
// two cases an attacker can actually produce — still do identical work.
func TestPAT_TheOwnerCheckDoesNotCostAnUnknownSelectorAnything(t *testing.T) {
	t.Parallel()
	s, _, _ := newSvc(t)
	tok, _ := mustBootstrap(t, s)
	ctx := context.Background()
	good := mustPAT(t, s, tok, "timing-owner")
	sel, _, _ := splitPAT(good.Secret)

	before := s.PATCompareCount()
	_, _ = s.VerifyPAT(ctx, PATPrefix+"zzzzzzzzzzzz.bbbbbbbbbbbb")
	unknown := s.PATCompareCount() - before

	before = s.PATCompareCount()
	_, _ = s.VerifyPAT(ctx, PATPrefix+sel+".wrong-secret")
	wrong := s.PATCompareCount() - before

	if unknown != wrong {
		t.Fatalf("unknown selector did %d compares, wrong secret did %d; adding the owner "+
			"lookup has made the two paths distinguishable again", unknown, wrong)
	}
}
