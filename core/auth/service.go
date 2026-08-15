package auth

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// DefaultSessionTTL is the token lifetime unless overridden with
// WithSessionTTL. The config knob arrives with the M5 server wiring.
const DefaultSessionTTL = 24 * time.Hour

// Identity is an authenticated caller, as resolved from a session token.
type Identity struct {
	UserID int64
	Name   string
	Role   string
}

// Service is the security core: one instance per process over one meta
// store. Safe for concurrent use.
type Service struct {
	store *meta.Store

	mu sync.Mutex
	mk []byte // unwrapped master key; nil while locked

	now       func() time.Time
	ttl       time.Duration
	cfgAllows []netip.Prefix // config-seeded allowlist (store rows add to it)
}

// Option configures a Service at New time.
type Option func(*Service)

// WithNow injects a clock (tests).
func WithNow(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithSessionTTL overrides DefaultSessionTTL.
func WithSessionTTL(d time.Duration) Option { return func(s *Service) { s.ttl = d } }

// WithConfigAllowlist seeds the IP allowlist from configuration CIDRs
// (config.Security.IPAllowlist). Invalid entries are rejected at New —
// config validation should have caught them earlier.
func WithConfigAllowlist(cidrs []string) Option {
	return func(s *Service) {
		for _, c := range cidrs {
			if p, err := netip.ParsePrefix(c); err == nil {
				s.cfgAllows = append(s.cfgAllows, p)
			}
		}
	}
}

// New builds the Service. It starts locked (ADR-0054 §1).
func New(store *meta.Store, opts ...Option) *Service {
	s := &Service{store: store, now: time.Now, ttl: DefaultSessionTTL}
	for _, o := range opts {
		o(s)
	}
	return s
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

// adoptMasterKey installs an unwrapped master key, verifying consistency
// against an already-unlocked key (a mismatch means a corrupted keyslot).
func (s *Service) adoptMasterKey(mk []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mk == nil {
		s.mk = mk
		return nil
	}
	if subtle.ConstantTimeCompare(s.mk, mk) != 1 {
		return ErrKeyslotCorrupt
	}
	return nil
}

// EncryptSecret seals plaintext under the install master key (AES-256-GCM).
// Used for connection DSNs (Objective 11). ErrLocked before first login.
func (s *Service) EncryptSecret(plaintext []byte) ([]byte, error) {
	mk, err := s.masterKey()
	if err != nil {
		return nil, err
	}
	return seal(mk, plaintext, aadSecret)
}

// DecryptSecret opens an EncryptSecret blob. ErrLocked before first login.
func (s *Service) DecryptSecret(blob []byte) ([]byte, error) {
	mk, err := s.masterKey()
	if err != nil {
		return nil, err
	}
	return open(mk, blob, aadSecret)
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

// AddAllowedIP persists an allowlist CIDR (admin only).
func (s *Service) AddAllowedIP(ctx context.Context, actor Identity, cidr, note, ip string) error {
	if actor.Role != meta.RoleAdmin {
		return ErrDenied
	}
	if _, err := netip.ParsePrefix(cidr); err != nil {
		return fmt.Errorf("auth: invalid CIDR %q: %w", cidr, err)
	}
	if _, err := s.store.AllowedIPs.OnCtx(ctx).
		Set(meta.IPCIDR, cidr).Set(meta.IPNote, note).
		Set(meta.IPCreatedBy, actor.UserID).Set(meta.IPCreatedAt, s.now().Unix()).
		Insert(); err != nil {
		return err
	}
	return s.Audit(ctx, actor.UserID, ip, "allowlist_added", cidr)
}

// RemoveAllowedIP deletes an allowlist CIDR (admin only).
func (s *Service) RemoveAllowedIP(ctx context.Context, actor Identity, cidr, ip string) error {
	if actor.Role != meta.RoleAdmin {
		return ErrDenied
	}
	if err := s.store.AllowedIPs.OnCtx(ctx).With(meta.IPCIDR, cidr).Delete(); err != nil {
		return err
	}
	return s.Audit(ctx, actor.UserID, ip, "allowlist_removed", cidr)
}

// Audit appends one always-on audit row (ADR-0054 §4). userID 0 records a
// pre-auth event. A failing audit write fails the calling operation.
func (s *Service) Audit(ctx context.Context, userID int64, ip, action, detail string) error {
	_, err := s.store.Audit.OnCtx(ctx).
		Set(meta.AuditUserID, userID).Set(meta.AuditIP, ip).
		Set(meta.AuditAction, action).Set(meta.AuditDetail, detail).
		Set(meta.AuditCreatedAt, s.now().Unix()).
		Insert()
	if err != nil {
		return fmt.Errorf("auth: audit write failed: %w", err)
	}
	return nil
}
