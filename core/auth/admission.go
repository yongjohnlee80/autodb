package auth

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// IP admission for the front door and the web UI (ADR-0075 §4, Amendment 1).
//
// Admission is (global allowlist OR the user's own rows), and a PAT's
// allowed_ips then NARROWS that, if it sets any.
//
// OR rather than AND, and this was Johno's ruling with the trade-off on the
// table. The global list carries shared infrastructure — office egress, a
// VPC, production application hosts — and under AND a colleague sitting at an
// already-listed office would still need a personal row before they could
// work. Worse, a home address would have to be added GLOBALLY to be usable,
// which bloats the perimeter for everyone and makes the per-user layer a
// second registration of the same fact rather than a narrowing of it.
//
// The accepted cost: a stolen PAT works from any globally-listed address, for
// any account. Per-token allowed_ips is the mitigation for a credential that
// warrants one.

// AdmissionSource says WHICH layer admitted an address. It is audited, so an
// operator reading the trail can tell "came from the office" from "came from
// this person's own registered address" — a distinction that matters when
// deciding whether an access was expected.
type AdmissionSource string

const (
	// AdmittedByGlobal means a config CIDR or an ip_allowlist row matched.
	AdmittedByGlobal AdmissionSource = "global"
	// AdmittedByUserRow means one of the user's own rows matched.
	AdmittedByUserRow AdmissionSource = "user-row"
	// NotAdmitted means neither layer matched.
	// AdmittedByTokenList is cleartext debugging mode ONLY (ADR-0086 §10):
	// the token's own allowed_ips was the entire admission gate and the
	// inherited set was not consulted. Its own value because the audit trail
	// must be able to say which sessions were admitted this way — they are the
	// ones whose perimeter was never checked against their owner's.
	AdmittedByTokenList AdmissionSource = "token-list"

	NotAdmitted AdmissionSource = "none"
)

// IPAllowedForUser reports whether ip may be used by userID, and which layer
// admitted it.
//
// The two layers are checked in the cheaper order — the global list is
// usually a handful of prefixes already in memory, the user's rows are a
// query — but the ORDER IS NOT A SHORTCUT: both are consulted when the first
// misses, because either alone is sufficient.
func (s *Service) IPAllowedForUser(ctx context.Context, tx *dao.Transaction, userID int64, ip string) (AdmissionSource, error) {
	s.admissionQueries.Add(1)
	if ip == LocalPeer {
		// A unix-socket peer has no address to match, and the 0600 socket is
		// itself the boundary (ADR-0058). This surface never sees one — the
		// front door is TCP-only — but the web gateway shares this predicate
		// and might.
		return AdmittedByGlobal, nil
	}
	addr, perr := netip.ParseAddr(ip)
	if perr != nil {
		return NotAdmitted, nil
	}
	addr = addr.Unmap()

	global, err := s.IPAllowed(ctx, ip)
	if err != nil {
		return NotAdmitted, err
	}
	if global {
		return AdmittedByGlobal, nil
	}

	rows, err := s.store.UserIPs.On(tx, dao.WithQueryContext(ctx)).
		With(meta.UIPUserID, userID).Select()
	if err != nil {
		return NotAdmitted, fmt.Errorf("auth: reading the user's allowlist: %w", err)
	}
	for _, r := range rows {
		p, pe := netip.ParsePrefix(r.CIDR)
		if pe != nil {
			// A malformed stored row must not break the check for every
			// other row — the same tolerance the global list already has.
			continue
		}
		if p.Contains(addr) {
			return AdmittedByUserRow, nil
		}
	}
	return NotAdmitted, nil
}

// PATAllowsIP reports whether a token's own allowed_ips admits ip.
//
// Empty means INHERIT — the token adds no narrowing and the admission set
// stands. It emphatically does not mean "nowhere": an empty list that denied
// everything would make the ordinary token, which is the common case, useless.
//
// A token that DOES set allowed_ips narrows: the address must fall inside one
// of its prefixes as well as being admitted above. That is the intersection
// the ADR describes, and it is the mitigation for the accepted cost of OR.
func PATAllowsIP(allowedIPs, ip string) bool {
	if strings.TrimSpace(allowedIPs) == "" {
		return true
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, raw := range strings.Split(allowedIPs, ",") {
		p, pe := netip.ParsePrefix(strings.TrimSpace(raw))
		if pe != nil {
			continue
		}
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
