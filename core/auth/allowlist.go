package auth

// The allowlist management surface (ADR-0075 §4). Two layers, two owners:
//
//   - The GLOBAL allowlist (config CIDRs ∪ ip_allowlist rows) gates every
//     login (Objective 21). Admin-managed; config entries are read-only at
//     runtime because silently narrowing an allowlist is not acceptable
//     (lector M3) — they are listed so an admin can SEE the whole truth,
//     but only store rows can be added or removed here.
//   - The PER-USER allowlist (user_ip_allowlist) is the front door's second
//     layer: a front-door login must pass both, and a PAT's allowed_ips
//     must be a subset of the owner's rows. Users manage their OWN rows
//     (self-service); admins manage anyone's. Every mutation is audited.

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/golib/dao"
)

// maxUserIPs bounds one user's allowlist rows — a positive safe default in
// the house style (ADR-0074 §1b): large enough for real setups (home, VPN,
// office, laptop…), small enough that an unbounded insert loop is refused.
const maxUserIPs = 32

// requireSelfOrAdmin resolves the token and demands that the actor either
// IS userID or holds the admin role. Deny-before-disclose: a non-admin
// probing another user's rows learns only ErrDenied, never whether the
// user exists.
func (s *Service) requireSelfOrAdmin(ctx context.Context, token string, userID int64) (Identity, error) {
	ident, _, err := s.resolveToken(ctx, token)
	if err != nil {
		return Identity{}, err
	}
	if ident.userID != userID && ident.role != meta.RoleAdmin {
		return Identity{}, ErrDenied
	}
	return ident, nil
}

// ListAllowedIPs returns the global allowlist as an admin sees it: the
// read-only config CIDRs and the managed store rows (admin token).
func (s *Service) ListAllowedIPs(ctx context.Context, token string) (config []string, rows []*meta.AllowedIP, err error) {
	if _, err := s.requireAdmin(ctx, token); err != nil {
		return nil, nil, err
	}
	for _, p := range s.cfgAllows {
		config = append(config, p.String())
	}
	rows, err = s.store.AllowedIPs.OnCtx(ctx).Select()
	if err != nil {
		return nil, nil, fmt.Errorf("auth: reading ip_allowlist: %w", err)
	}
	return config, rows, nil
}

// UserIPs lists userID's per-user allowlist rows (self or admin token).
func (s *Service) UserIPs(ctx context.Context, token string, userID int64) ([]*meta.UserIP, error) {
	if _, err := s.requireSelfOrAdmin(ctx, token, userID); err != nil {
		return nil, err
	}
	rows, err := s.store.UserIPs.OnCtx(ctx).With(meta.UIPUserID, userID).Select()
	if err != nil {
		return nil, fmt.Errorf("auth: reading user_ip_allowlist: %w", err)
	}
	return rows, nil
}

// canonicalCIDR accepts either a CIDR or a bare address (the self-service
// "add the IP I am connecting from" path hands us a bare address) and
// returns the canonical single-address prefix for the latter.
func canonicalCIDR(cidr string) (string, error) {
	if p, err := netip.ParsePrefix(cidr); err == nil {
		// A 4-in-6 mapped prefix canonicalizes to its IPv4 form, exactly as
		// a bare mapped address does — otherwise ::ffff:10.1.2.3/128 and
		// 10.1.2.3/32 would be two "different" rows for one network
		// (lector PR #21 r0 SF1). A mapped prefix wider than the mapped
		// space (< /96) spans more than IPv4 and is refused as meaningless
		// for an allowlist entry.
		if p.Addr().Is4In6() {
			if p.Bits() < 96 {
				return "", fmt.Errorf("auth: IPv4-mapped prefix %q wider than the mapped space", cidr)
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		return p.Masked().String(), nil
	}
	addr, err := netip.ParseAddr(cidr)
	if err != nil {
		return "", fmt.Errorf("auth: invalid CIDR or address %q", cidr)
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()).String(), nil
}

// AddUserIP inserts one per-user allowlist row (self or admin token). cidr
// may be a bare address; it is canonicalized. The per-user cap and the
// UNIQUE(user_id, cidr) constraint both refuse inside the same transaction
// that writes the audit row.
func (s *Service) AddUserIP(ctx context.Context, token string, userID int64, cidr, label, ip string) error {
	actor, err := s.requireSelfOrAdmin(ctx, token, userID)
	if err != nil {
		return err
	}
	canon, err := canonicalCIDR(cidr)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *dao.Transaction) error {
		// Serialize concurrent adds for this user by taking a WRITE LOCK on
		// the owner's users row first: count-then-insert alone is not
		// concurrency-safe on PostgreSQL under READ COMMITTED — lector
		// reproduced 35 rows against cap 32 from concurrent adds (PR #21 r0
		// MF1); sqlite only masked it by serializing writers. Touching
		// updated_at is the lock's visible form, and is also semantically
		// true: the user's security posture changed.
		if err := s.store.Users.On(tx).With(meta.UserID, userID).
			Set(meta.UserUpdatedAt, s.now().Unix()).Update(); err != nil {
			return err
		}
		existing, err := s.store.UserIPs.On(tx).With(meta.UIPUserID, userID).Select()
		if err != nil {
			return err
		}
		if len(existing) >= maxUserIPs {
			return fmt.Errorf("auth: user %d already holds %d allowlist rows (cap %d)", userID, len(existing), maxUserIPs)
		}
		if _, err := s.store.UserIPs.On(tx).
			Set(meta.UIPUserID, userID).Set(meta.UIPCIDR, canon).
			Set(meta.UIPLabel, label).Set(meta.UIPCreatedAt, s.now().Unix()).
			Insert(); err != nil {
			return err
		}
		return s.AuditTx(tx, actor.userID, ip, "user_ip_added",
			fmt.Sprintf("user %d: %s", userID, canon))
	})
}

// RemoveUserIP deletes one of userID's rows by row id (self or admin
// token). The delete is scoped to (id AND user_id) so a row id can never
// reach across into another user's allowlist.
func (s *Service) RemoveUserIP(ctx context.Context, token string, userID, rowID int64, ip string) error {
	actor, err := s.requireSelfOrAdmin(ctx, token, userID)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *dao.Transaction) error {
		// Fetch-before-delete: a (id, user_id) miss is a silent no-op and
		// MUST NOT write an audit row — a false "user_ip_removed" entry
		// asserts a security change that never happened (lector PR #21 r0
		// MF2, proven by control on VM43). The fetch also lets the audit
		// name the CIDR rather than an opaque row id.
		row, err := s.store.UserIPs.On(tx).
			With(meta.UIPID, rowID).With(meta.UIPUserID, userID).Get()
		if errors.Is(err, dao.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.store.UserIPs.On(tx).
			With(meta.UIPID, rowID).With(meta.UIPUserID, userID).
			Delete(); err != nil {
			return err
		}
		return s.AuditTx(tx, actor.userID, ip, "user_ip_removed",
			fmt.Sprintf("user %d: %s", userID, row.CIDR))
	})
}
