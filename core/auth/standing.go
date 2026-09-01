package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// STANDING AUTHORITY: what a session's right to continue rests on, re-read
// fresh (ADR-0075 Amendment 4's F3a seam; lector's ruling on the PAT
// standing-authority defect).
//
// A pinned transaction outlives the call that opened it, so the authority
// behind it has to be re-checkable later without a token. There are two kinds
// of credential and they live in different tables — an auth-session row for
// the interactive surfaces, a PAT row for the front door — and the previous
// code assumed the first was the only one.
//
// It did not assume it explicitly, which is why nothing caught it: wire
// sessions stored a zero session id as a sentinel meaning "no session row",
// the janitor passed that zero to a lookup keyed on session id, and the
// missing row read as a revocation. Every front-door transaction would have
// been rolled back and closed on the first sweep, audited as though someone
// had withdrawn permission. It was unreachable only because no wire session
// could open a transaction yet.
//
// So the reference is TYPED. A caller cannot store a bare id any more, and a
// resolver cannot guess which table it came from.

// AuthorityKind distinguishes the two credentials a session can stand on.
type AuthorityKind string

const (
	// AuthoritySession is an interactive login: the authority is a session
	// row, and revocation and expiry live on it.
	AuthoritySession AuthorityKind = "auth-session"
	// AuthorityPAT is a front-door connection: the authority is a personal
	// access token, and revocation and expiry live on the token record.
	AuthorityPAT AuthorityKind = "pat"
)

// AuthorityRef names the durable row a session's authority rests on.
//
// The zero value is deliberately NOT a valid reference: a session that has
// not said what it stands on cannot be re-checked, and silently treating that
// as "no constraints" is the defect this type exists to make unrepresentable.
type AuthorityRef struct {
	Kind AuthorityKind
	// ID is the stable row id — a session id for AuthoritySession, a PAT id
	// for AuthorityPAT.
	ID int64
}

// SessionAuthority and PATAuthority build the two forms.
func SessionAuthority(id int64) AuthorityRef { return AuthorityRef{Kind: AuthoritySession, ID: id} }
func PATAuthority(id int64) AuthorityRef     { return AuthorityRef{Kind: AuthorityPAT, ID: id} }

// Valid reports whether the reference names something re-checkable.
func (r AuthorityRef) Valid() bool {
	return r.ID > 0 && (r.Kind == AuthoritySession || r.Kind == AuthorityPAT)
}

func (r AuthorityRef) String() string {
	if !r.Valid() {
		return "authority(unset)"
	}
	return fmt.Sprintf("%s:%d", r.Kind, r.ID)
}

// StandingVerdict is what a re-check found. It is STRUCTURED rather than a
// single error, because the caller's correct response differs: a revoked
// credential ends the session, while a demotion ends the transaction and
// leaves a still-entitled reader connected.
//
// Collapsing both into ErrDenied is what the previous shape did, and it is
// why a demotion could only ever be handled as a revocation.
type StandingVerdict struct {
	// Standing reports that the credential, its owner and the target are all
	// still good enough to hold a session at the read floor.
	Standing bool
	// MayWrite reports whether the effective role still clears ActionWrite.
	// False with Standing true is the demotion case.
	MayWrite bool
	// Role is the effective account role at this moment, for the audit.
	Role string
	// Identity is the caller, resolved from the same read that decided the
	// verdict.
	//
	// Carried here because the alternative is for every caller to re-read the
	// user row to build one, and a second read is a second answer: the row
	// can change between them, and then the unit runs as one identity and is
	// authorized as another. One read, one answer.
	Identity Identity
	// Reason names why Standing is false, for the audit trail only.
	Reason string
}

// Standing-failure reasons. Each is a distinct operator-visible cause; the
// caller turns all of them into the same teardown.
const (
	StandingCredentialGone    = "credential-missing"
	StandingCredentialRevoked = "credential-revoked"
	StandingCredentialExpired = "credential-expired"
	StandingOwnerDisabled     = "owner-disabled"
	StandingNoGrant           = "grant-removed"
	StandingUnreferenced      = "authority-unreferenced"
)

// ResolveStanding re-reads a session's authority and reports what it may
// still do.
//
// THE ONE RESOLVER. The janitor and every per-operation authorization on the
// wire share it, because two implementations of "may this session continue"
// drift, and the direction they drift in is the one nobody notices — a
// background sweep that is stricter than the foreground check merely kills
// sessions, while a foreground check that is laxer than the sweep serves
// statements the sweep would have refused.
//
// A store failure is returned as an ERROR rather than as a non-standing
// verdict. It is not a revocation, and treating it as one would turn a blip
// in the meta store into rolled-back transactions across every open session.
func (s *Service) ResolveStanding(ctx context.Context, ref AuthorityRef, userID, connID int64) (StandingVerdict, error) {
	if !ref.Valid() {
		// A session that never said what it stands on. Refusing is the only
		// safe reading: the alternative is to treat an unset reference as
		// unlimited authority, which is exactly the shape of the defect.
		return StandingVerdict{Reason: StandingUnreferenced}, nil
	}

	switch ref.Kind {
	case AuthoritySession:
		if v, err := s.standingFromSession(ctx, ref.ID, userID); !v.Standing || err != nil {
			return v, err
		}
	case AuthorityPAT:
		if v, err := s.standingFromPAT(ctx, ref.ID, userID); !v.Standing || err != nil {
			return v, err
		}
	default:
		return StandingVerdict{Reason: StandingUnreferenced}, nil
	}

	// The owner and the grant are the same question for both kinds, so they
	// are asked once here rather than twice above.
	u, err := s.store.Users.OnCtx(ctx).With(meta.UserID, userID).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return StandingVerdict{Reason: StandingCredentialGone}, nil
	}
	if err != nil {
		return StandingVerdict{}, err
	}
	if u.Disabled != 0 {
		return StandingVerdict{Reason: StandingOwnerDisabled}, nil
	}

	g, err := s.store.Grants.OnCtx(ctx).
		With(meta.GrantUserID, userID).With(meta.GrantConnID, connID).Get()
	if errors.Is(err, dao.ErrNoRows) {
		// NO GRANT MEANS NO STANDING, for an admin too.
		//
		// The first version let an admin through here on the reasoning that
		// admin is an account-level power a grant does not delegate. That is
		// true of ActionManage and false of connection access: decide()
		// requires a grant for read and write whatever the account role, so
		// an admin with no grant row cannot BEGIN — and letting one KEEP a
		// transaction the authorizer would refuse to start is a wider
		// authority than the system grants anywhere else.
		//
		// The existing revocation cell caught it, which is the whole reason
		// that cell walks every way an authority can end rather than the one
		// the author had in mind.
		return StandingVerdict{Reason: StandingNoGrant}, nil
	}
	if err != nil {
		return StandingVerdict{}, err
	}

	eff := min(rankOf(u.Role), rankOf(g.Role))
	if eff < requiredRank(ActionRead) {
		return StandingVerdict{Reason: StandingNoGrant}, nil
	}
	return StandingVerdict{
		Standing: true,
		MayWrite: eff >= requiredRank(ActionWrite),
		Role:     u.Role,
		Identity: Identity{userID: u.ID, name: u.Name, role: u.Role},
	}, nil
}

// standingFromSession checks an interactive login's own row.
func (s *Service) standingFromSession(ctx context.Context, sessID, userID int64) (StandingVerdict, error) {
	row, err := s.store.Sessions.OnCtx(ctx).With(meta.SessID, sessID).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return StandingVerdict{Reason: StandingCredentialGone}, nil
	}
	if err != nil {
		return StandingVerdict{}, err
	}
	switch {
	case row.UserID != userID:
		return StandingVerdict{Reason: StandingCredentialGone}, nil
	case row.Revoked != 0:
		return StandingVerdict{Reason: StandingCredentialRevoked}, nil
	case s.now().Unix() >= row.ExpiresAt:
		return StandingVerdict{Reason: StandingCredentialExpired}, nil
	}
	return StandingVerdict{Standing: true}, nil
}

// standingFromPAT checks a front-door token's own row.
//
// The same three questions as a session's, asked of the table the answer
// actually lives in. Nothing here reads the token itself: the reference is a
// row id, so a live secret never has to be retained for the life of a
// transaction in order to re-check it later.
func (s *Service) standingFromPAT(ctx context.Context, patID, userID int64) (StandingVerdict, error) {
	row, err := s.store.PATs.OnCtx(ctx).With(meta.PATID, patID).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return StandingVerdict{Reason: StandingCredentialGone}, nil
	}
	if err != nil {
		return StandingVerdict{}, err
	}
	switch {
	case row.UserID != userID:
		return StandingVerdict{Reason: StandingCredentialGone}, nil
	case row.Revoked != 0:
		return StandingVerdict{Reason: StandingCredentialRevoked}, nil
	case row.ExpiresAt != 0 && s.now().Unix() >= row.ExpiresAt:
		return StandingVerdict{Reason: StandingCredentialExpired}, nil
	}
	return StandingVerdict{Standing: true}, nil
}
