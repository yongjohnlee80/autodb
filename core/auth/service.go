package auth

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// DefaultSessionTTL is the token lifetime unless overridden with
// WithSessionTTL. The config knob arrives with the M5 server wiring.
const DefaultSessionTTL = 24 * time.Hour

// adminGuardKey is the store_meta row used as a transaction-scoped lock that
// serializes security-invariant mutations (bootstrap, admin demote/disable/
// remove) across processes — the DB-level fix for check-then-act races on
// READ COMMITTED engines (ADR-0054 rev 1, lector M3 must-fix #3).
const adminGuardKey = "admin_guard"

// Identity is an authenticated caller as resolved from a session token at
// call time. Fields are unexported so no caller can forge one — only this
// package constructs identities, from token resolution (lector M3
// must-fix #1).
type Identity struct {
	userID int64
	name   string
	role   string
}

// UserID reports the account id.
func (i Identity) UserID() int64 { return i.userID }

// Name reports the account name.
func (i Identity) Name() string { return i.name }

// Role reports the account's global role as of token resolution.
func (i Identity) Role() string { return i.role }

// Service is the security core: one instance per process over one meta
// store. Methods are safe for concurrent use within the process; the
// security-critical invariants (bootstrap-once, last-enabled-admin) are
// additionally serialized at the database so concurrent processes cannot
// race them.
type Service struct {
	store *meta.Store

	mu sync.Mutex
	mk []byte // unwrapped master key; nil while locked

	now       func() time.Time
	ttl       time.Duration
	cfgAllows []netip.Prefix
}

// Option configures a Service at New time.
type Option func(*Service) error

// WithNow injects a clock (tests).
func WithNow(now func() time.Time) Option {
	return func(s *Service) error { s.now = now; return nil }
}

// WithSessionTTL overrides DefaultSessionTTL.
func WithSessionTTL(d time.Duration) Option {
	return func(s *Service) error {
		if d <= 0 {
			return fmt.Errorf("auth: session TTL must be positive, got %v", d)
		}
		s.ttl = d
		return nil
	}
}

// WithConfigAllowlist seeds the IP allowlist from configuration CIDRs. An
// invalid CIDR fails New loudly — config validation should have caught it,
// and silently narrowing an allowlist is not acceptable (lector M3
// should-fix).
func WithConfigAllowlist(cidrs []string) Option {
	return func(s *Service) error {
		for _, c := range cidrs {
			p, err := netip.ParsePrefix(c)
			if err != nil {
				return fmt.Errorf("auth: invalid allowlist CIDR %q: %w", c, err)
			}
			s.cfgAllows = append(s.cfgAllows, p)
		}
		return nil
	}
}

// New builds the Service. It starts locked (ADR-0054 §1).
func New(store *meta.Store, opts ...Option) (*Service, error) {
	s := &Service{store: store, now: time.Now, ttl: DefaultSessionTTL}
	for _, o := range opts {
		if err := o(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Unlocked reports whether the master key is available in this process.
func (s *Service) Unlocked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mk != nil
}

// masterKey returns the unwrapped master key or ErrLocked.
func (s *Service) masterKey() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mk == nil {
		return nil, ErrLocked
	}
	return s.mk, nil
}

// checkKeyConsistency verifies mk matches an already-unlocked master key
// WITHOUT installing it — called before any state is committed, so a
// corrupted keyslot cannot half-succeed (lector M3 must-fix #2 ordering).
func (s *Service) checkKeyConsistency(mk []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mk != nil && subtle.ConstantTimeCompare(s.mk, mk) != 1 {
		return ErrKeyslotCorrupt
	}
	return nil
}

// adoptMasterKey installs an unwrapped master key AFTER the surrounding
// mutation committed. checkKeyConsistency must have passed first.
func (s *Service) adoptMasterKey(mk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mk == nil {
		s.mk = mk
	}
}

// inTx runs fn inside one meta-store transaction — every security mutation
// and its audit row commit atomically (lector M3 must-fix #2).
func (s *Service) inTx(ctx context.Context, fn func(tx *dao.Transaction) error) error {
	return dao.RunTx(ctx, []dao.DataConn{s.store.Conn()}, fn)
}

// lockGuardRow serializes invariant-critical transactions across processes
// by upserting the store_meta guard row inside tx — the second transaction
// blocks on the row lock until the first commits, so its in-tx rechecks see
// committed truth (must-fix #3).
func (s *Service) lockGuardRow(tx *dao.Transaction) error {
	return s.store.KV.On(tx).
		Set(meta.KVKey, adminGuardKey).Set(meta.KVValue, "1").Upsert()
}

// AuditTx appends one audit row inside tx (atomic with its mutation).
func (s *Service) AuditTx(tx *dao.Transaction, userID int64, ip, action, detail string) error {
	_, err := s.store.Audit.On(tx).
		Set(meta.AuditUserID, userID).Set(meta.AuditIP, ip).
		Set(meta.AuditAction, action).Set(meta.AuditDetail, detail).
		Set(meta.AuditCreatedAt, s.now().Unix()).
		Insert()
	if err != nil {
		return fmt.Errorf("auth: audit write failed: %w", err)
	}
	return nil
}

// Audit appends one standalone audit row (no accompanying mutation — e.g.
// failed logins, rejected executions). Callers with a mutation in flight
// must use the transactional path instead.
func (s *Service) Audit(ctx context.Context, userID int64, ip, action, detail string) error {
	return s.inTx(ctx, func(tx *dao.Transaction) error {
		return s.AuditTx(tx, userID, ip, action, detail)
	})
}

// connAAD binds a connection secret's ciphertext to its row identity so a
// meta-DB writer cannot swap ciphertexts between rows (must-fix #5).
func connAAD(connID int64) string { return fmt.Sprintf("autodb:conn:%d:v1", connID) }

// EncryptSecret seals a connection secret under the install master key,
// bound to the connection's id. ErrLocked before first login.
func (s *Service) EncryptSecret(plaintext []byte, connID int64) ([]byte, error) {
	mk, err := s.masterKey()
	if err != nil {
		return nil, err
	}
	return seal(mk, plaintext, connAAD(connID))
}

// DecryptSecret opens an EncryptSecret blob for the given connection id. A
// blob copied from another connection's row fails authentication.
func (s *Service) DecryptSecret(blob []byte, connID int64) ([]byte, error) {
	mk, err := s.masterKey()
	if err != nil {
		return nil, err
	}
	return open(mk, blob, connAAD(connID))
}

// IPAllowed reports whether ip falls inside the allowlist — the union of
// config CIDRs and ip_allowlist rows (Objective 21). A malformed ip is
// never allowed.
func (s *Service) IPAllowed(ctx context.Context, ip string) (bool, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false, nil
	}
	addr = addr.Unmap()
	for _, p := range s.cfgAllows {
		if p.Contains(addr) {
			return true, nil
		}
	}
	rows, err := s.store.AllowedIPs.OnCtx(ctx).Select()
	if err != nil {
		return false, fmt.Errorf("auth: reading ip_allowlist: %w", err)
	}
	for _, r := range rows {
		p, perr := netip.ParsePrefix(r.CIDR)
		if perr != nil {
			continue // malformed stored row must not break the check
		}
		if p.Contains(addr) {
			return true, nil
		}
	}
	return false, nil
}

// AddAllowedIP persists an allowlist CIDR (admin token).
func (s *Service) AddAllowedIP(ctx context.Context, token, cidr, note, ip string) error {
	actor, err := s.requireAdmin(ctx, token)
	if err != nil {
		return err
	}
	if _, err := netip.ParsePrefix(cidr); err != nil {
		return fmt.Errorf("auth: invalid CIDR %q: %w", cidr, err)
	}
	return s.inTx(ctx, func(tx *dao.Transaction) error {
		if _, err := s.store.AllowedIPs.On(tx).
			Set(meta.IPCIDR, cidr).Set(meta.IPNote, note).
			Set(meta.IPCreatedBy, actor.userID).Set(meta.IPCreatedAt, s.now().Unix()).
			Insert(); err != nil {
			return err
		}
		return s.AuditTx(tx, actor.userID, ip, "allowlist_added", cidr)
	})
}

// RemoveAllowedIP deletes an allowlist CIDR (admin token).
func (s *Service) RemoveAllowedIP(ctx context.Context, token, cidr, ip string) error {
	actor, err := s.requireAdmin(ctx, token)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *dao.Transaction) error {
		if err := s.store.AllowedIPs.On(tx).With(meta.IPCIDR, cidr).Delete(); err != nil {
			return err
		}
		return s.AuditTx(tx, actor.userID, ip, "allowlist_removed", cidr)
	})
}
