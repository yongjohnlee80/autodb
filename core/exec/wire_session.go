package exec

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// The front door's authentication chain (protocol matrix row 2.7).
//
// Every check below runs, then ONE atomic reservation. The order is the
// matrix's and it is not arbitrary: the cheap, purely local checks come
// before the queries, and the target is validated before any capacity is
// taken, so a connection that was never going to be served does not briefly
// hold a lease.
//
// WHICH CHECK FAILED IS AUDIT-ONLY. The wire gets one shape for all of them.
// A caller who could distinguish "no such database" from "no grant" from
// "wrong token" could map the install without ever holding a credential —
// slowly, but nobody notices slow.

// WireSessionResult is what a successful authentication produced.
type WireSessionResult struct {
	SessionID SessionID
	UserID    int64
	ConnID    int64
	// AdmissionSource records WHICH allowlist layer admitted the address
	// (ADR-0075 Amendment 1). Audited, so an operator can tell a connection
	// from shared infrastructure apart from one from a person's own
	// registered address.
	AdmissionSource auth.AdmissionSource
	// PATName is the credential that authenticated, for the audit trail:
	// who, under whose account, with which token.
	PATName string
	// UserName is the CANONICAL owner name, not the client's spelling of
	// it. The startup `user` parameter matched it case-insensitively (step
	// 2), so echoing the parameter back as session_authorization would let
	// the client's casing become the session's identity — and the identity
	// a session reports should be the one the grants are written against.
	UserName string
}

// wireDenial carries the INTERNAL reason for an audit row. The caller turns
// every one of these into the same wire denial.
type wireDenial struct{ reason string }

func (e wireDenial) Error() string { return "frontdoor: " + e.reason }

// DenialReason extracts the internal reason from an OpenWireSession error, or
// "" if the error is not a denial (a store failure, say, which must not be
// reported as an authentication problem).
func DenialReason(err error) string {
	var d wireDenial
	if errors.As(err, &d) {
		return d.reason
	}
	return ""
}

func deny(reason string) error { return wireDenial{reason: reason} }

// WireDenial builds a denial with the given internal reason.
//
// Exported as the pair of DenialReason: the package that owns the reason
// vocabulary owns how one is constructed, so a caller in another package
// cannot invent a denial shape this package would not recognise. The front
// door's cells build one to prove the wire treats every reason identically.
func WireDenial(reason string) error { return deny(reason) }

// The internal denial reasons. Each names a distinct operator-visible cause;
// none of them ever reaches the wire.
const (
	DenyBadCredential  = "frontdoor/bad-credential"
	DenyUserMismatch   = "frontdoor/startup-user-mismatch"
	DenyIPNotAdmitted  = "frontdoor/ip-not-admitted"
	DenyPATIPNarrowed  = "frontdoor/pat-allowed-ips"
	DenyNoSuchDatabase = "frontdoor/no-such-database"
	DenyNoGrant        = "frontdoor/no-grant"
	DenyProfileRefuses = "frontdoor/profile-not-front-door"
	DenyLeaseCap       = "frontdoor/lease-cap-exceeded"
	DenySessionCap     = "frontdoor/session-cap-exceeded"
	DenyResidentBudget = "frontdoor/resident-budget-exceeded"
)

// WireSessionOverhead is the fixed memory charged for one wire session: the
// ExecSession's own state — the session record, its registry and per-user
// entries, the reservation itself, and the bookkeeping the engine keeps for
// the session's whole life.
//
// IT DOES NOT COVER ANY WIRE-SIDE BUFFER, and the first version of this
// comment said it did — "its decoder, TLS record buffers and session
// bookkeeping". That was wrong and it was load-bearing wrong: the front door
// separately reserves 64 KiB per connection for its control lane, whose
// comment also mentions the decoder, so the two read as one charge taken
// twice. They are different terms of ADR-0075 §8.4's worst case, charged
// against different budgets by different packages. The protocol matrix §8.5
// carries the allocation-to-charge map; lector found the drift on PR #36.
//
// A flat figure rather than a measurement, deliberately, and lector's PR #33
// ruling accepts it as such: the budget's job is to bound the total, and a
// charge that varied with actual allocation would let a session grow past
// what it reserved — the reservation would stop meaning anything at the
// moment it was most needed. Variable input, retained and output allocations
// stay separately charged where they are made.
const WireSessionOverhead = 64 * 1024

// OpenWireSession authenticates a front-door connection and reserves its
// capacity, returning the session it may then use.
//
// presented is the PAT from the PasswordMessage; startupUser and database are
// the StartupMessage parameters; ip is the canonical peer address.
func (e *Engine) OpenWireSession(ctx context.Context, presented, startupUser, database, ip string) (WireSessionResult, error) {
	var out WireSessionResult

	// 1. The credential. VerifyPAT already collapses unknown/wrong/revoked/
	// expired/disabled-owner into one error and does comparable work across
	// the paths an attacker can produce.
	pat, err := e.auth.VerifyPAT(ctx, presented)
	if err != nil {
		if errors.Is(err, auth.ErrPATInvalid) {
			return out, deny(DenyBadCredential)
		}
		// A store failure is not a bad credential.
		return out, err
	}

	// 2. The startup `user` must be the token's owner. Identity comes from
	// the TOKEN; this parameter is a cross-check, never an override — a
	// client that names someone else is either confused or probing.
	owner, err := e.store.Users.OnCtx(ctx).With(meta.UserID, pat.UserID).Get()
	if err != nil {
		return out, err
	}
	if !strings.EqualFold(owner.Name, startupUser) {
		return out, deny(DenyUserMismatch)
	}

	// 3. IP admission: (global ∨ the user's rows), then the token's own
	// narrowing if it sets one (Amendment 1).
	src, err := e.auth.IPAllowedForUser(ctx, nil, pat.UserID, ip)
	if err != nil {
		return out, err
	}
	if src == auth.NotAdmitted {
		return out, deny(DenyIPNotAdmitted)
	}
	if !auth.PATAllowsIP(pat.AllowedIPs, ip) {
		return out, deny(DenyPATIPNarrowed)
	}

	// 4. The target. `database` names an autodb CONNECTION, by name or as
	// conn:<id>.
	connRow, err := e.lookupWireTarget(ctx, database)
	if err != nil {
		if errors.Is(err, dao.ErrNoRows) {
			return out, deny(DenyNoSuchDatabase)
		}
		return out, err
	}
	// A grant on that connection. Read is the floor for holding a session;
	// what the session may then RUN is decided per statement.
	if aerr := e.auth.AuthorizeUser(ctx, pat.UserID, connRow.ID, auth.ActionRead); aerr != nil {
		if errors.Is(aerr, auth.ErrDenied) {
			return out, deny(DenyNoGrant)
		}
		return out, aerr
	}
	// And the connection must admit front-door use at all. Opt-in per
	// connection (ADR-0075 §1): no target is reachable through this surface
	// by default, so adding the listener does not silently expose every
	// database the daemon knows about.
	if e.profileFor(connRow) != ProfileSession {
		return out, deny(DenyProfileRefuses)
	}

	// 5. ONE atomic reservation: per-user slot, global slot, target lease,
	// and this session's fixed overhead.
	id, err := newSessionID()
	if err != nil {
		return out, err
	}
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &session{
		id: id, userID: pat.UserID, authSessID: 0, connID: connRow.ID,
		ctx: sctx, cancel: cancel, lastUsed: e.now(),
	}
	if rerr := e.sessions.admitWithLease(s, connRow.ID, WireSessionOverhead); rerr != nil {
		cancel()
		switch {
		case errors.Is(rerr, ErrLeaseCapExceeded):
			return out, deny(DenyLeaseCap)
		case errors.Is(rerr, ErrSessionCapExceeded):
			return out, deny(DenySessionCap)
		case errors.Is(rerr, ErrResidentBudgetExceeded):
			return out, deny(DenyResidentBudget)
		}
		return out, rerr
	}

	e.auth.NotePATUse(ctx, pat)
	return WireSessionResult{
		SessionID: id, UserID: pat.UserID, ConnID: connRow.ID,
		AdmissionSource: src, PATName: pat.Name, UserName: owner.Name,
	}, nil
}

// lookupWireTarget resolves the DSN's database field to a connection row.
//
// Two spellings: the connection's name, or `conn:<id>`. The id form exists
// because a name can be changed and a DSN in a config file cannot notice.
func (e *Engine) lookupWireTarget(ctx context.Context, database string) (*meta.Connection, error) {
	database = strings.TrimSpace(database)
	if rest, ok := strings.CutPrefix(database, "conn:"); ok {
		id, perr := strconv.ParseInt(rest, 10, 64)
		if perr != nil {
			return nil, dao.ErrNoRows
		}
		return e.store.Connections.OnCtx(ctx).With(meta.ConnID, id).Get()
	}
	return e.store.Connections.OnCtx(ctx).With(meta.ConnName, database).Get()
}

// CloseWireSession releases a front-door session and everything it reserved.
func (e *Engine) CloseWireSession(ctx context.Context, id SessionID, userID int64, ip, reason string) {
	s, err := e.sessions.lookup(id, userID)
	if err != nil {
		return
	}
	e.closeSession(ctx, s, ip, reason)
}
