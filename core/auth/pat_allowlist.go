package auth

// The mint path may add the caller's OWN allowlist rows, once they have said so.
//
// This is not a new kind of permission. A row created here is an ordinary
// user_ip_allowlist row with the ordinary lifecycle: it is managed and removed
// through the same self-service surface as any other, it survives the token
// that occasioned it, and the token never owns it. What this file adds is an
// ADD-POINT and the arithmetic that keeps it honest.
//
// Why it exists: a developer restricting a short-lived token to the address
// they are sitting at was refused, because a token's allowed_ips must be a
// subset of its owner's rows -- a token cannot widen where its owner may
// connect from. The refusal was correct and unhelpful: the fix is to let them
// widen their OWN admission deliberately, not to let a token escape it.
//
// Why it needs care: a row added here admits that user's PASSWORD LOGINS and
// every inherit-empty PAT they hold, from that address, and it keeps doing so
// after this token expires. So the operation is confirmed against an exact set,
// and the set is re-checked under the owner's lock before anything is written.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/golib/dao"
)

var (
	// ErrApprovalStale — the rows that would be added are no longer the rows
	// the operator was shown and approved.
	//
	// A TYPED outcome rather than a message, because the caller has to tell
	// this apart from every other failure in order to RE-PROMPT rather than
	// report a fault. Control flow from codes, not from prose.
	ErrApprovalStale = errors.New("auth: the allowlist additions needing confirmation have changed")

	// ErrPATDebugNoWiden — a cleartext-debugging token never widens the user
	// allowlist.
	//
	// Its own non-empty, narrow allowed_ips list is the ENTIRE gate for that
	// credential class, and CreatePAT deliberately skips the subset check for
	// it. Adding those CIDRs to the user's rows is unnecessary for the token
	// AND would manufacture standing admission for later TLS and password
	// use -- exposure in a class the debug token was never granted.
	ErrPATDebugNoWiden = errors.New("auth: a cleartext-debugging token cannot add allowlist rows")
)

// StaleApproval carries the recomputed set, so a caller can show exactly what
// changed instead of asking the operator to guess.
type StaleApproval struct {
	// Missing is the canonical set of CIDRs that would now be added.
	Missing []string
}

// Error returns a formatted error message detailing the stale approval state.
// StaleApproval implements the error interface.
func (e *StaleApproval) Error() string {
	return fmt.Sprintf("%v: now %s", ErrApprovalStale, strings.Join(e.Missing, ", "))
}

// Unwrap lets errors.Is(err, ErrApprovalStale) succeed while errors.As reaches
// the set.
func (e *StaleApproval) Unwrap() error { return ErrApprovalStale }

// canonicalCIDRSet canonicalizes and DEDUPLICATES a requested list.
//
// Deduplication first, because everything downstream counts: a list naming the
// same prefix twice must not consume two cap slots or produce two audit rows.
func canonicalCIDRSet(cidrs []string) ([]string, error) {
	seen := map[string]struct{}{}
	var out []string
	for _, raw := range cidrs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		// canonicalCIDR, NOT a second parser.
		//
		// net.ParseCIDR alone rejected a BARE ADDRESS -- and the form accepts
		// and forwards bare addresses, so the very input this feature was
		// asked for (203.0.113.7) failed at preview. canonicalCIDR is the
		// established one: it takes both forms and gives v4/v6/4-in-6 parity,
		// which a fresh parser here would have to re-derive and would get
		// subtly wrong.
		c, err := canonicalCIDR(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrPATBadAllowedIPs, err)
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

// ownedNetsTx reads the owner's rows inside the transaction. Read under the
// owner's lock, so the containment computed from it is the containment that
// will hold at commit.
func (s *Service) ownedNetsTx(ctx context.Context, tx *dao.Transaction, userID int64) (rows []*meta.UserIP, nets []*net.IPNet, err error) {
	// A NIL TRANSACTION IS THE PREVIEW, and it needs the non-transactional
	// accessor rather than On(nil): the preview is a read with no mutation to
	// serialize, and handing a nil transaction to On() is not the same thing
	// as having none.
	if tx == nil {
		rows, err = s.store.UserIPs.OnCtx(ctx).With(meta.UIPUserID, userID).Select()
	} else {
		rows, err = s.store.UserIPs.On(tx, dao.WithQueryContext(ctx)).
			With(meta.UIPUserID, userID).Select()
	}
	if err != nil {
		return nil, nil, err
	}
	for _, r := range rows {
		if _, n, perr := net.ParseCIDR(r.CIDR); perr == nil {
			nets = append(nets, n)
		}
		// A malformed stored row is skipped rather than fatal, the same
		// tolerance the admission predicate already has: one bad row must not
		// break the check for every other.
	}
	return rows, nets, nil
}

// missingFrom returns the canonical CIDRs not already contained by owned.
//
// CONTAINED, not equal: an address inside an existing row needs no new row,
// creates none, relabels nothing and consumes no cap slot. That is the
// property that keeps a repeated mint from growing the allowlist.
func missingFrom(canon []string, owned []*net.IPNet) []string {
	var out []string
	for _, c := range canon {
		// Already canonical, so this parse cannot fail on input the set
		// produced; a bare address became a /32 or /128 upstream.
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			continue
		}
		if !containedInAny(n, owned) {
			out = append(out, c)
		}
	}
	return out
}

// subsetOf reports whether every element of small is in big.
func subsetOf(small, big []string) bool {
	set := make(map[string]struct{}, len(big))
	for _, b := range big {
		set[b] = struct{}{}
	}
	for _, s := range small {
		if _, ok := set[s]; !ok {
			return false
		}
	}
	return true
}

// PATAllowlistAdditions previews what minting with these restrictions would
// ADD to the caller's own allowlist, so the confirmation can name the exact
// set rather than a category.
//
// PRESENTATION ONLY. The committing transaction recomputes this under the
// owner's lock and may add nothing that is not in the set the operator then
// approved; a preview is a proposal, never a permission. Returning an empty
// set means the operation does not widen and needs no confirmation.
func (s *Service) PATAllowlistAdditions(ctx context.Context, token string, cidrs []string) ([]string, error) {
	ident, _, err := s.resolveToken(ctx, token)
	if err != nil {
		return nil, err
	}
	canon, err := canonicalCIDRSet(cidrs)
	if err != nil {
		return nil, err
	}
	if len(canon) == 0 {
		return nil, nil
	}
	_, owned, err := s.ownedNetsTx(ctx, nil, ident.UserID())
	if err != nil {
		return nil, err
	}
	return missingFrom(canon, owned), nil
}

// resolveMintAllowlist decides, under the owner's lock, what this mint may
// write: the token's canonical allowed_ips string and the rows to create.
//
// The three outcomes are distinct on purpose:
//
//   - nothing missing            -> no rows, and no confirmation was needed;
//   - missing, none approved     -> today's refusal, unchanged, so a caller
//     that never opted in cannot widen anything;
//   - missing, approved exactly  -> those rows, and only those.
//
// The approved set is compared against the RECOMPUTED missing set, not against
// the request. Those differ: if one requested CIDR was already contained when
// the prompt was drawn, the prompt showed the others -- and if its containing
// row is removed in the meantime, "in the request" would silently admit a CIDR
// the operator was never shown. A set that SHRANK is fine (something became
// contained; adding fewer rows than approved needs no new consent); a set that
// grew is refused with the recomputed set attached.
func (s *Service) resolveMintAllowlist(
	ctx context.Context, tx *dao.Transaction, userID int64,
	requested []string, approved []string,
) (canonical string, toAdd []string, err error) {
	canon, err := canonicalCIDRSet(requested)
	if err != nil {
		return "", nil, err
	}
	if len(canon) == 0 {
		// Empty means INHERIT the owner's admission set, which is the common
		// case and adds nothing.
		return "", nil, nil
	}

	_, owned, err := s.ownedNetsTx(ctx, tx, userID)
	if err != nil {
		return "", nil, err
	}
	missing := missingFrom(canon, owned)
	canonical = strings.Join(canon, ",")

	switch {
	case len(missing) == 0:
		return canonical, nil, nil
	case len(approved) == 0:
		// The message is the one this path has always given, because the
		// situation is the same one: a token cannot widen its owner.
		return "", nil, fmt.Errorf("%w: %s is not inside any of your own allowlist rows; a token "+
			"cannot widen where its owner may connect from", ErrPATBadAllowedIPs, missing[0])
	case !subsetOf(missing, approved):
		return "", nil, &StaleApproval{Missing: missing}
	default:
		return canonical, missing, nil
	}
}

// addMintAllowlistRows creates the approved rows inside the mint's
// transaction, with provenance, and audits each one.
//
// The label records the PAT's IMMUTABLE ID and a SNAPSHOTTED name. A name
// alone is insufficient: names are reusable, and the token may be gone by the
// time somebody reads the row and decides whether to remove it. The selector,
// the secret and its hash never appear -- a row label is not a place for a
// credential.
//
// The cap is enforced HERE, inside the lock, against a re-read count: a check
// against a count read before the lock is a check against the past.
func (s *Service) addMintAllowlistRows(
	tx *dao.Transaction, userID, patID int64, patName string, toAdd []string, ip string,
) error {
	if len(toAdd) == 0 {
		return nil
	}
	existing, err := s.store.UserIPs.On(tx).With(meta.UIPUserID, userID).Select()
	if err != nil {
		return err
	}
	if len(existing)+len(toAdd) > maxUserIPs {
		// Refused as one operation: adding some of the rows would leave a
		// token whose restriction its owner's admission cannot satisfy.
		return fmt.Errorf("auth: user %d holds %d allowlist rows and %d more would exceed the "+
			"cap of %d; remove some first", userID, len(existing), len(toAdd), maxUserIPs)
	}
	label := fmt.Sprintf("added when minting token #%d %q", patID, patName)
	now := s.now().Unix()
	for _, cidr := range toAdd {
		if _, ierr := s.store.UserIPs.On(tx).
			Set(meta.UIPUserID, userID).Set(meta.UIPCIDR, cidr).
			Set(meta.UIPLabel, label).Set(meta.UIPCreatedAt, now).
			Insert(); ierr != nil {
			return ierr
		}
		// One audit per row ACTUALLY created, naming the immutable PAT id so
		// the two records correlate. An already-contained CIDR reaches here
		// never, so it produces no audit claiming a change that did not
		// happen.
		if aerr := s.AuditTx(tx, userID, ip, "user_ip_added",
			fmt.Sprintf("user %d: %s (minting token #%d)", userID, cidr, patID)); aerr != nil {
			return aerr
		}
	}
	return nil
}
