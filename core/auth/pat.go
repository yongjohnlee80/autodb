package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// Personal Access Tokens — the front door's credential (ADR-0075 §4).
//
// A PAT is what a person pastes into a DSN. That single fact drives most of
// what follows: it is long-lived because a DSN is not re-typed daily, it is
// named because a person with several needs to know which is which, and it is
// revocable because a credential that lives in configuration files will
// eventually be somewhere it should not be.
//
// It is deliberately NOT the login passphrase. The passphrase unwraps the
// user's encryption keyslot (ADR-0054), so a leaked passphrase is total
// compromise; a leaked PAT is bounded — gated queries, allowlisted IPs, TLS,
// audited, and revocable in one command.

// PAT errors.
var (
	// ErrPATInvalid is the ONE external failure for every credential
	// problem: unknown selector, wrong secret, revoked, expired. Callers
	// cannot tell them apart, deliberately.
	ErrPATInvalid = errors.New("auth: invalid access token")

	// ErrPATNameTaken reports a duplicate name for one user. This one IS
	// distinguishable, and safely so: it is returned to an authenticated
	// caller managing their own tokens, not to an anonymous peer.
	ErrPATNameTaken = errors.New("auth: you already have a token with that name")

	// ErrPATCapExceeded reports the per-user or global active-token cap.
	ErrPATCapExceeded = errors.New("auth: active token cap reached")

	// ErrPATBadExpiry reports an expiry outside the permitted window.
	ErrPATBadExpiry = errors.New("auth: token lifetime out of range")

	// ErrPATBadAllowedIPs reports allowed_ips that are unparseable or not a
	// subset of the user's own allowlist rows.
	ErrPATBadAllowedIPs = errors.New("auth: token allowed_ips rejected")

	// ErrPATNotFound reports a revoke for a name the user does not have.
	//
	// Its own sentinel rather than a wrapped dao.ErrNoRows, because the wire
	// layer maps sentinels: an unmapped store error reaches the caller as a
	// -32603 internal fault, which tells someone who mistyped a token name
	// that the SERVER broke. It is safe to name — this reaches an
	// authenticated caller managing their own tokens.
	ErrPATNotFound = errors.New("auth: no token with that name")
)

// PAT policy (ADR-0075 §4 defaults table).
const (
	// PATMaxPerUser and PATMaxGlobal bound ACTIVE tokens. Explicit 0 is a
	// configuration error elsewhere; these are the defaults.
	PATMaxPerUser = 16
	PATMaxGlobal  = 512

	// PATDefaultLifetime and PATMaxLifetime bound expiry. Non-expiring
	// tokens do not exist: a credential in a config file outlives the
	// person's memory of it, so the only question is whether it expires on a
	// schedule or when someone finally notices.
	PATDefaultLifetime = 90 * 24 * time.Hour
	PATMaxLifetime     = 365 * 24 * time.Hour

	// patSelectorBytes and patSecretBytes size the two halves. The selector
	// is a lookup key, not a secret; the secret is 32 bytes of CSPRNG.
	patSelectorBytes = 9
	patSecretBytes   = 32

	// PATPrefix marks the credential in logs, config files and leak
	// scanners. A recognisable prefix is what lets a secret scanner find one
	// in a commit before anybody else does.
	PATPrefix = "adb_pat_"
)

// testAfterCapLock runs inside CreatePAT's check→insert window. Test-only.
func (s *Service) SetTestAfterCapLock(h func()) { s.testAfterCapLock = h }

// NewPAT is a freshly created token. Secret is present exactly once, here.
type NewPAT struct {
	Name string
	// Secret is the full credential: prefix, selector, secret. Shown once
	// and never recoverable — the store keeps only the selector and a hash.
	Secret    string
	ExpiresAt time.Time
}

// patHash is the stored digest of a secret half.
func patHash(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// splitPAT separates a presented credential into selector and secret.
//
// A malformed credential still yields a selector and a secret (both possibly
// empty) rather than an early error, because the caller must do the same work
// for a malformed token as for a wrong one — see VerifyPAT.
func splitPAT(token string) (selector, secret string, wellFormed bool) {
	rest, ok := strings.CutPrefix(token, PATPrefix)
	if !ok {
		return "", "", false
	}
	sel, sec, ok := strings.Cut(rest, ".")
	if !ok {
		return "", "", false
	}
	return sel, sec, true
}

// CreatePAT issues a token for the calling user.
//
// The secret is returned once. Everything durable is the selector and a
// SHA-256 of the secret, so the store cannot reconstruct the credential even
// if it is read in full — which is the property that makes a database backup
// something other than a credential dump.
func (s *Service) CreatePAT(ctx context.Context, token, name string, lifetime time.Duration, allowedIPs []string) (NewPAT, error) {
	ident, err := s.ValidateToken(ctx, token)
	if err != nil {
		return NewPAT{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return NewPAT{}, fmt.Errorf("%w: a token needs a name so you can tell it from the others "+
			"when you come to revoke one", ErrPATNameTaken)
	}
	if lifetime == 0 {
		lifetime = PATDefaultLifetime
	}
	if lifetime < 0 || lifetime > PATMaxLifetime {
		return NewPAT{}, fmt.Errorf("%w: %s requested, maximum is %s and there is no non-expiring "+
			"form — a credential in a config file outlives anyone's memory of it",
			ErrPATBadExpiry, lifetime, PATMaxLifetime)
	}

	selector, err := randomToken(patSelectorBytes)
	if err != nil {
		return NewPAT{}, err
	}
	secret, err := randomToken(patSecretBytes)
	if err != nil {
		return NewPAT{}, err
	}
	now := s.now()
	expires := now.Add(lifetime)

	var canonical string
	// SERIALIZE, then count, then insert.
	//
	// One transaction is not mutual exclusion. Under PostgreSQL READ
	// COMMITTED, concurrent creates each read the same committed count, each
	// find a free slot, and each insert — the transaction gives atomicity of
	// the write, not exclusivity of the decision. I claimed in the first
	// version that one transaction closed this gap; it did not, and lector
	// reproduced 19 active tokens against a cap of 16.
	//
	// The locks come first and ALWAYS in this order — global guard row, then
	// the owner's users row. A consistent order is what stops two
	// transactions taking them in opposite sequences and deadlocking.
	//
	// This is the pattern PR #21 already established for the per-user
	// allowlist cap, after lector reproduced the same defect there (35 rows
	// against a cap of 32). It was sitting in this package while I wrote the
	// unsafe version.
	err = s.inTx(ctx, func(tx *dao.Transaction) error {
		if lerr := s.lockGuardRow(tx); lerr != nil {
			return lerr
		}
		// Touching updated_at is the owner-row lock's visible form, and it
		// is semantically true: the user's credential set is changing.
		if lerr := s.store.Users.On(tx).With(meta.UserID, ident.UserID()).
			Set(meta.UserUpdatedAt, now.Unix()).Update(); lerr != nil {
			return lerr
		}

		// The subset read joins the SAME transaction, so the rows it
		// validates against are the ones this decision is made on.
		var cerr error
		canonical, cerr = s.canonicalAllowedIPs(ctx, tx, ident.UserID(), allowedIPs)
		if cerr != nil {
			return cerr
		}

		if h := s.testAfterCapLock; h != nil {
			// Inside the window: the locks are held and nothing has been
			// counted yet. A hook here widens the check→insert gap on
			// demand, which is what makes the cap race DETERMINISTIC rather
			// than a matter of how the scheduler felt — the
			// concurrency-testing convention's "inject the competing
			// transition inside the window" applied to this decision.
			//
			// With the locks in place a competitor blocks here, which is
			// the property under test. Without them it sails through, and
			// the cell sees the overrun every time instead of occasionally.
			h()
		}
		mine, cerr := s.store.PATs.On(tx).With(meta.PATUserID, ident.UserID()).
			With(meta.PATRevoked, int64(0)).Count()
		if cerr != nil {
			return cerr
		}
		if mine >= PATMaxPerUser {
			return fmt.Errorf("%w: you have %d active tokens and the limit is %d; revoke one first",
				ErrPATCapExceeded, mine, PATMaxPerUser)
		}
		all, cerr := s.store.PATs.On(tx).With(meta.PATRevoked, int64(0)).Count()
		if cerr != nil {
			return cerr
		}
		if all >= PATMaxGlobal {
			return fmt.Errorf("%w: this install is at its limit of %d active tokens",
				ErrPATCapExceeded, PATMaxGlobal)
		}
		if _, ierr := s.store.PATs.On(tx).
			Set(meta.PATSelector, selector).
			Set(meta.PATSecretHash, patHash(secret)).
			Set(meta.PATUserID, ident.UserID()).
			Set(meta.PATName, name).
			Set(meta.PATAllowedIPs, canonical).
			Set(meta.PATCreatedAt, now.Unix()).
			Set(meta.PATExpiresAt, expires.Unix()).
			Insert(); ierr != nil {
			if errors.Is(ierr, dao.ErrDuplicate) {
				return fmt.Errorf("%w: %q", ErrPATNameTaken, name)
			}
			return ierr
		}
		// The audit rides the SAME transaction as the insert: a token that
		// exists with no record of its creation is exactly the token an
		// investigation cannot account for.
		return s.AuditTx(tx, ident.UserID(), "", "pat_created",
			fmt.Sprintf("name %q expires %s", name, expires.UTC().Format(time.RFC3339)))
	})
	if err != nil {
		return NewPAT{}, err
	}
	return NewPAT{Name: name, Secret: PATPrefix + selector + "." + secret, ExpiresAt: expires}, nil
}

// canonicalAllowedIPs validates a token's own narrowing and returns it
// canonicalized for storage.
//
// Empty is legal and means "inherit the admission set" (ADR-0075
// Amendment 1). A non-empty list must be a SUBSET of the user's own rows:
// a token cannot widen where its owner may connect from, or the per-user
// layer would be advisory.
//
// One consequence worth naming, because it looks like a bug from the inside:
// a user whose admission comes only from the GLOBAL list — an office egress
// address, say, with no personal rows — has nothing for a subset to be taken
// of, so they cannot narrow a token by IP at all. They leave it empty and
// inherit. That is the amendment's own shape rather than an oversight here:
// subset-at-creation is against the user's OWN rows, and a global address is
// not theirs to carve up.
// It takes an OPTIONAL transaction and issues through On(tx) — nil is the
// pool by contract. That shape is the autodb executor convention and it is
// not academic here: F0d's atomic reservation will want this subset check
// while holding a transaction, and an OnCtx inside a helper called with a
// resource held crosses a pinned connection with a pool call. Fixing the
// signature now costs one parameter; discovering it from inside the
// reservation would cost a review round and a puzzling deadlock.
func (s *Service) canonicalAllowedIPs(ctx context.Context, tx *dao.Transaction, userID int64, cidrs []string) (string, error) {
	if len(cidrs) == 0 {
		return "", nil
	}
	rows, err := s.store.UserIPs.On(tx, dao.WithQueryContext(ctx)).
		With(meta.UIPUserID, userID).Select()
	if err != nil {
		return "", err
	}
	var owned []*net.IPNet
	for _, r := range rows {
		if _, n, perr := net.ParseCIDR(r.CIDR); perr == nil {
			owned = append(owned, n)
		}
	}
	var out []string
	for _, raw := range cidrs {
		_, n, perr := net.ParseCIDR(strings.TrimSpace(raw))
		if perr != nil {
			return "", fmt.Errorf("%w: %q is not a CIDR", ErrPATBadAllowedIPs, raw)
		}
		if !containedInAny(n, owned) {
			return "", fmt.Errorf("%w: %s is not inside any of your own allowlist rows; a token "+
				"cannot widen where its owner may connect from", ErrPATBadAllowedIPs, n)
		}
		out = append(out, n.String())
	}
	sort.Strings(out)
	return strings.Join(out, ","), nil
}

// containedInAny reports whether n is inside one of the outer networks.
func containedInAny(n *net.IPNet, outer []*net.IPNet) bool {
	nOnes, nBits := n.Mask.Size()
	for _, o := range outer {
		oOnes, oBits := o.Mask.Size()
		if nBits != oBits || nOnes < oOnes {
			// A shorter prefix is a WIDER network; it cannot be a subset.
			continue
		}
		if o.Contains(n.IP) {
			return true
		}
	}
	return false
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generating a token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

var _ = subtle.ConstantTimeCompare

// VerifyPAT resolves a presented credential to its owner.
//
// EVERY failure returns ErrPATInvalid: unknown selector, wrong secret,
// revoked, expired, malformed. The caller cannot tell them apart, and neither
// can anyone watching — which is the harder half, and the reason this
// function is shaped the way it is.
//
// Comparable work on every path (ADR-0075 §4). An unknown selector still
// performs a SHA-256 and a constant-time compare against a decoy digest. The
// obvious implementation returns early when the lookup misses, and that early
// return is a measurable difference: an attacker submits a candidate username
// or token and learns from the RESPONSE TIME whether the selector existed,
// without ever holding a valid credential. Enumeration by clock is still
// enumeration.
//
// The comparison is constant-time for the same reason, one level down: a
// byte-wise compare that stops at the first difference leaks how much of a
// guess was right.
func (s *Service) VerifyPAT(ctx context.Context, presented string) (*meta.PAT, error) {
	selector, secret, wellFormed := splitPAT(presented)

	var row *meta.PAT
	if wellFormed {
		got, err := s.store.PATs.OnCtx(ctx).With(meta.PATSelector, selector).Get()
		switch {
		case err == nil:
			row = got
		case errors.Is(err, dao.ErrNoRows):
			// Fall through to the decoy work below.
		default:
			// A store failure is NOT an invalid credential. Reporting it as
			// one would turn a database blip into "your token is bad",
			// sending an operator to rotate a credential that was fine.
			return nil, err
		}
	}

	// The decoy. Its digest is a real SHA-256 of a real 32-byte value, so
	// the work is the work — not a sleep approximating it, which would drift
	// from the true cost the moment either side changed.
	stored := decoyDigest
	if row != nil {
		stored = row.SecretHash
	}
	s.patCompares.Add(1)
	match := subtle.ConstantTimeCompare(patHash(secret), stored) == 1

	now := s.now()
	switch {
	case row == nil, !match:
		return nil, ErrPATInvalid
	case row.IsRevoked():
		return nil, ErrPATInvalid
	case now.Unix() >= row.ExpiresAt:
		return nil, ErrPATInvalid
	}

	// The OWNER's current state, re-read on every call.
	//
	// Without this a disabled account's PAT kept working: SetUserDisabled
	// revokes sessions, not tokens, so a credential sitting in a DSN
	// outlived the account it belonged to and offboarding was incomplete.
	// Session tokens already guarantee token -> live session -> live enabled
	// user on every call; a PAT is a longer-lived credential and needs the
	// same guarantee more, not less.
	//
	// Re-read rather than revoked-at-disable-time on purpose. Revoking on
	// disable would leave two places that decide whether a credential works,
	// and the day they disagree is the day someone is disabled and still
	// connected. One authoritative check per call cannot drift.
	//
	// AFTER the match, deliberately. Everything above this point is
	// identical for an unknown selector and a wrong secret — the two cases
	// an attacker can actually produce — so the extra query cannot become a
	// timing oracle for which selectors exist. Reaching here at all requires
	// the real secret.
	owner, oerr := s.store.Users.OnCtx(ctx).With(meta.UserID, row.UserID).Get()
	switch {
	case errors.Is(oerr, dao.ErrNoRows):
		return nil, ErrPATInvalid
	case oerr != nil:
		// A store failure is not an invalid credential — reporting it as one
		// would send someone to rotate a token that was fine.
		return nil, oerr
	case owner.Disabled != 0:
		return nil, ErrPATInvalid
	}
	return row, nil
}

// decoyDigest is what an unknown selector is compared against.
//
// Generated once at startup from real randomness rather than being a constant
// of zeros: a fixed decoy is a value an attacker can compute, and a token
// whose secret hashed to it would authenticate as nobody — a small hole, but
// one that costs nothing to close.
var decoyDigest = func() []byte {
	b := make([]byte, patSecretBytes)
	if _, err := rand.Read(b); err != nil {
		// Startup with no entropy is not a state to continue from, but this
		// is a package initializer; the digest of a zero block is still a
		// valid comparison target and VerifyPAT still refuses everything.
		return patHash("")
	}
	return patHash(base64.RawURLEncoding.EncodeToString(b))
}()

// ListPATs returns a user's tokens. Never the secret — it does not exist
// anywhere to return.
func (s *Service) ListPATs(ctx context.Context, token string, userID int64) ([]*meta.PAT, error) {
	ident, err := s.ValidateToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if userID == 0 {
		userID = ident.UserID()
	}
	if userID != ident.UserID() && ident.Role() != meta.RoleAdmin {
		return nil, ErrDenied
	}
	return s.store.PATs.OnCtx(ctx).With(meta.PATUserID, userID).Select()
}

// RevokePAT revokes one of the caller's tokens by name; an admin may revoke
// anyone's.
//
// Revocation is a flag rather than a delete: the row is what an audit trail
// points AT, and deleting it would leave every "authenticated with token X"
// record naming something that no longer exists. Offboarding is one command
// precisely because the record survives it.
func (s *Service) RevokePAT(ctx context.Context, token string, userID int64, name string) error {
	ident, err := s.ValidateToken(ctx, token)
	if err != nil {
		return err
	}
	if userID == 0 {
		userID = ident.UserID()
	}
	if userID != ident.UserID() && ident.Role() != meta.RoleAdmin {
		return ErrDenied
	}
	return s.inTx(ctx, func(tx *dao.Transaction) error {
		row, gerr := s.store.PATs.On(tx).With(meta.PATUserID, userID).With(meta.PATName, name).Get()
		if errors.Is(gerr, dao.ErrNoRows) {
			return fmt.Errorf("%w: %q", ErrPATNotFound, name)
		}
		if gerr != nil {
			return gerr
		}
		if uerr := s.store.PATs.On(tx).With(meta.PATID, row.ID).
			Set(meta.PATRevoked, int64(1)).Update(); uerr != nil {
			return uerr
		}
		return s.AuditTx(tx, ident.UserID(), "", "pat_revoked",
			fmt.Sprintf("user %d token %q", userID, name))
	})
}

// patCompares counts hash-and-compare operations, so a test can assert that
// an unknown selector performs the SAME work as a wrong secret.
//
// A counter and not a stopwatch, because the stopwatch could not see it. The
// difference an early return makes here is one SHA-256 and one 32-byte
// compare — around a microsecond — against a store lookup and scheduler
// noise that are orders of magnitude larger. A timing test with a tolerance
// loose enough to be stable on a shared runner is far too loose to resolve
// that, and I confirmed it: a version that returned early on an unknown
// selector passed the timing cell. An instrument that cannot observe the
// defect it is aimed at is not evidence, whatever colour it reports.
//
// PER-SERVICE and not package-global. As a package variable it was read as a
// delta around one call while OTHER parallel tests were incrementing it, so
// the cell reported 2 compares against 1 and failed in CI while passing
// everywhere I had run it. A shared counter read as a delta is not an
// instrument, it is a race — the same class of mistake as the shared mutable
// test knob that -race caught in R5.

// PATCompareCount reads this service's counter. Test-support, exported
// because the front-door package needs it once the auth chain lands.
func (s *Service) PATCompareCount() int64 { return s.patCompares.Load() }

// AuthorizeUser is Authorize for a caller already identified WITHOUT a
// session token — the front door, whose identity comes from a PAT.
//
// It exists because Authorize starts from a token and resolves a session
// row, and a PAT is deliberately not a session: it has no session row to
// resolve. The grant logic itself is identical and is not duplicated —
// there is one place that decides whether a role and a grant clear an
// action, and both entry points reach it.
func (s *Service) AuthorizeUser(ctx context.Context, userID, connID int64, action Action) error {
	u, err := s.store.Users.OnCtx(ctx).With(meta.UserID, userID).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	if u.Disabled != 0 {
		return ErrDenied
	}
	if action == ActionManage {
		if u.Role != meta.RoleAdmin {
			return ErrDenied
		}
		return nil
	}
	g, err := s.store.Grants.OnCtx(ctx).
		With(meta.GrantUserID, userID).With(meta.GrantConnID, connID).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	if min(rankOf(u.Role), rankOf(g.Role)) < requiredRank(action) {
		return ErrDenied
	}
	return nil
}

// PATLastUsedInterval is how stale a token's last_used may get before a
// successful authentication refreshes it.
//
// Coalescing is the point (ADR-0075 §4). last_used exists so an operator can
// see which tokens are live when deciding what to revoke; that question is
// answered just as well by "used within the last few minutes" as by a
// to-the-second timestamp, and the to-the-second version would mean a WRITE
// on a path that must stay cheap. An app's connection pool reconnecting in a
// loop would otherwise turn a diagnostic column into steady write load on the
// meta store.
const PATLastUsedInterval = 5 * time.Minute

// NotePATUse records that a token authenticated, at most once per interval.
//
// Best-effort by design: this is a diagnostic, and failing an authentication
// that has already succeeded because a bookkeeping write failed would trade a
// working connection for a nicer audit column.
func (s *Service) NotePATUse(ctx context.Context, pat *meta.PAT) {
	now := s.now()
	if now.Sub(time.Unix(pat.LastUsedAt, 0)) < PATLastUsedInterval {
		return
	}
	if err := s.store.PATs.OnCtx(ctx).With(meta.PATID, pat.ID).
		Set(meta.PATLastUsedAt, now.Unix()).Update(); err != nil {
		// Nothing to escalate to: the caller is mid-authentication and this
		// is a column nobody authenticates against.
		return
	}
	pat.LastUsedAt = now.Unix()
}
