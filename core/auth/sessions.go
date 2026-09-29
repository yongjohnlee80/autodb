package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/meta"
)

// tokenHash maps the opaque token string to its stored digest — the store
// never holds a usable token (Objective 20).
func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// resolveToken is the single provenance check: token → live session → live
// user → fresh Identity. Every privileged method calls it, so authority is
// re-read from the store on every call — a demotion or disable takes effect
// on the caller's next request. caller is who presents the token (Caller).
func (s *Service) resolveToken(ctx context.Context, caller Caller, token string) (Identity, *meta.Session, error) {
	return s.checkSession(reader{ctx: ctx}, caller, token)
}

// resolveTokenTx is resolveToken bound to an open transaction. Callers
// already inside a transaction MUST use this: resolving on the pool while a
// transaction holds a connection deadlocks single-connection stores
// (sqlite), and it would read outside the transaction's snapshot.
func (s *Service) resolveTokenTx(tx *dao.Transaction, caller Caller, token string) (Identity, error) {
	ident, _, err := s.checkSession(reader{tx: tx}, caller, token)
	return ident, err
}

// checkSession is both resolvers' one body, run on the reader it is given.
//
// Beyond the session and its user being live, it holds a token to the
// surface it was minted for. A session bound to a remote device
// (sessions.device_id) is refused on the local surface. On the remote
// surface only such a session is accepted, and only when:
//   - the connection proved that same device;
//   - the connection owns the session (sessions.attached_conn);
//   - the device, and the SSH key it was enrolled with, are not revoked.
//
// All of it is read on the same reader, so a revocation takes effect on the
// caller's next request, inside an open transaction too. Every refusal is
// ErrTokenInvalid: which check failed is not said.
func (s *Service) checkSession(r reader, caller Caller, token string) (Identity, *meta.Session, error) {
	sess, err := s.sessionsOn(r).With(meta.SessTokenHash, tokenHash(token)).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return Identity{}, nil, ErrTokenInvalid
	}
	if err != nil {
		return Identity{}, nil, err
	}
	if sess.Revoked != 0 || s.now().Unix() >= sess.ExpiresAt {
		return Identity{}, nil, ErrTokenInvalid
	}
	if err := s.checkSurface(r, caller, sess); err != nil {
		return Identity{}, nil, err
	}
	u, err := s.usersOn(r).With(meta.UserID, sess.UserID).Get()
	if err != nil {
		return Identity{}, nil, err
	}
	if u.Disabled != 0 {
		return Identity{}, nil, ErrTokenInvalid
	}
	return Identity{userID: u.ID, name: u.Name, role: u.Role}, sess, nil
}

// checkSurface is checkSession's surface rule for sess presented by caller.
func (s *Service) checkSurface(r reader, caller Caller, sess *meta.Session) error {
	if caller.Surface != SurfaceRemote {
		if sess.DeviceID != 0 {
			return ErrTokenInvalid
		}
		return nil
	}
	if sess.DeviceID == 0 || sess.DeviceID != caller.DeviceID {
		return ErrTokenInvalid
	}
	if caller.ConnID == "" || sess.AttachedConn != caller.ConnID {
		return ErrTokenInvalid
	}
	dev, err := s.devicesOn(r).With(meta.DevID, sess.DeviceID).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return ErrTokenInvalid
	}
	if err != nil {
		return err
	}
	if dev.RevokedAt != 0 || dev.UserID != sess.UserID {
		return ErrTokenInvalid
	}
	key, err := s.sshKeysOn(r).With(meta.SSHKeyID, dev.SSHKeyID).Get()
	if errors.Is(err, dao.ErrNoRows) {
		return ErrTokenInvalid
	}
	if err != nil {
		return err
	}
	if key.RevokedAt != 0 {
		return ErrTokenInvalid
	}
	return nil
}

// ValidateToken resolves a session token to a fresh Identity.
func (s *Service) ValidateToken(ctx context.Context, token string) (Identity, error) {
	ident, _, err := s.resolveToken(ctx, CallerFrom(ctx), token)
	return ident, err
}

// RequireAdmin authorizes a SERVER-scoped admin operation — one with no
// connection to grant against (currently: shutdown). The caller must hold
// a live token for an enabled admin; everything else is ErrDenied, which
// the wire renders without disclosing which check failed.
func (s *Service) RequireAdmin(ctx context.Context, token string) (Identity, error) {
	return s.requireAdmin(ctx, token)
}

// requireAdmin resolves the token and demands a current admin role.
func (s *Service) requireAdmin(ctx context.Context, token string) (Identity, error) {
	ident, _, err := s.resolveToken(ctx, CallerFrom(ctx), token)
	if err != nil {
		return Identity{}, err
	}
	if ident.role != meta.RoleAdmin {
		return Identity{}, ErrDenied
	}
	return ident, nil
}

// newSessionTx inserts a session row inside tx and returns the one-time
// token string.
func (s *Service) newSessionTx(tx *dao.Transaction, userID int64, ip string) (string, error) {
	token, _, err := s.newBoundSessionTx(tx, userID, ip, 0, "")
	return token, err
}

// newBoundSessionTx is newSessionTx for a session bound to remote device
// deviceID and owned by connection conn (both zero for a local session). It
// also returns the session's id.
func (s *Service) newBoundSessionTx(tx *dao.Transaction, userID int64, ip string, deviceID int64, conn string) (string, int64, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", 0, fmt.Errorf("auth: generating token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()
	id, err := s.store.Sessions.On(tx).
		Set(meta.SessTokenHash, tokenHash(token)).
		Set(meta.SessUserID, userID).Set(meta.SessIP, ip).
		Set(meta.SessCreatedAt, now.Unix()).
		Set(meta.SessExpiresAt, now.Add(s.ttl).Unix()).
		Set(meta.SessRevoked, int64(0)).
		Set(meta.SessDeviceID, deviceID).Set(meta.SessAttachedConn, conn).
		Insert()
	if err != nil {
		return "", 0, err
	}
	return token, id, nil
}

// LocalPeer is the pseudo-address a unix-domain (local) connection presents
// in place of an IP. A unix socket is created 0600 in a per-user runtime
// directory, so reaching it already proves same-user access — that is the
// boundary the socket design chose, and it is stronger than any IP allowlist, which
// exists for the TCP/remote case. A local connection therefore carries this
// marker rather than an IP, is recorded verbatim in the audit trail and
// session rows (honest: "local", not a fake 127.0.0.1), and is exempt from
// the allowlist gate in Login. It is a fixed sentinel, never a parseable
// address, so it can only be produced deliberately by the transport layer.
const LocalPeer = "local"

// Login authenticates name+passphrase from ip and issues a session token.
// The session insert and the audit row commit atomically; the master key is
// installed in memory only after the commit. Failures are
// audited and deliberately indistinguishable (ErrBadCredentials), except a
// disallowed IP (ErrDenied).
func (s *Service) Login(ctx context.Context, name, passphrase, ip string) (string, Identity, error) {
	return s.LoginAt(ctx, name, passphrase, ip, "")
}

// LoginAt is Login with an ADMISSION ADDRESS the caller observed on its own
// behalf — the browser's address, which the daemon cannot see because its own
// peer is the gateway over loopback.
//
// ONE OPERATION, and that is the whole point of it existing (
// ruling). The gateway used to log in, get a session back, ask a second RPC
// whether the address was admitted, and log the session out again when it was
// not. That sequence did strictly more work for a CORRECT password than for
// an incorrect one — minting and revoking a real session — and the difference
// was measurable: about 7ms on a 118ms request under the race detector,
// reproducible on every run. A caller at a refused address could confirm a
// guessed password by timing the refusal, without ever being let in.
//
// It looked like a choice between two leaks — judge the address first and
// leak which usernames exist, or judge it last and leak which password is
// right. It was not. The design requires the credential to be verified
// before the user-row lookup and the denial to stay uniform; it does not
// require a session to exist before the address is judged. Combining them
// here means the admission decision happens BETWEEN verification and
// minting, so a refused caller costs exactly what a wrong password costs and
// no session is ever created.
//
// An empty admissionIP means no second layer, which is the local TUI and
// every existing caller: Login is that, unchanged.
func (s *Service) LoginAt(ctx context.Context, name, passphrase, ip, admissionIP string) (string, Identity, error) {
	return s.login(ctx, name, passphrase, ip, admissionIP, nil)
}

// login is LoginAt's body, and LoginRemote's when remote is set.
func (s *Service) login(ctx context.Context, name, passphrase, ip, admissionIP string, remote *remoteSession) (string, Identity, error) {
	// A local (unix-socket) connection is exempt from the IP allowlist:
	// the 0600 socket is the boundary, and a socket peer has no
	// IP to match, so the allowlist can only ever refuse it. The allowlist
	// governs TCP peers, which carry a real address.
	//
	// A remote sign-in is exempt too: the SSH key and the device proof are
	// its gate, and signing in remotely is how a user adds a new address.
	if ip != LocalPeer && remote == nil {
		allowed, err := s.IPAllowed(ctx, ip)
		if err != nil {
			return "", Identity{}, err
		}
		if !allowed {
			if err := s.Audit(ctx, 0, ip, "login_failed", name+" (ip not allowed)"); err != nil {
				return "", Identity{}, err
			}
			return "", Identity{}, ErrDenied
		}
	}

	u, err := s.store.Users.OnCtx(ctx).With(meta.UserName, name).Get()
	if errors.Is(err, dao.ErrNoRows) {
		dummyDerive(passphrase)            // equalize timing for unknown users
		s.decoyAdmission(ctx, admissionIP) // and for the admission query below
		if aerr := s.Audit(ctx, 0, ip, "login_failed", name+" (unknown user)"); aerr != nil {
			return "", Identity{}, aerr
		}
		return "", Identity{}, ErrBadCredentials
	}
	if err != nil {
		return "", Identity{}, err
	}
	if u.Disabled != 0 {
		dummyDerive(passphrase)
		s.decoyAdmission(ctx, admissionIP)
		if aerr := s.Audit(ctx, u.ID, ip, "login_failed", name+" (disabled)"); aerr != nil {
			return "", Identity{}, aerr
		}
		return "", Identity{}, ErrBadCredentials
	}
	if remote != nil && u.ID != remote.rl.OwnerID {
		dummyDerive(passphrase)
		if aerr := s.Audit(ctx, u.ID, ip, "login_failed", name+" (not the SSH key's owner)"); aerr != nil {
			return "", Identity{}, aerr
		}
		return "", Identity{}, ErrRemoteUserMismatch
	}

	params, verifier, err := decodeHash(string(u.PassHash))
	if err != nil {
		return "", Identity{}, err
	}
	kek, authHalf := deriveKeys(passphrase, params)
	if !verifyAuthHalf(authHalf, verifier) {
		s.decoyAdmission(ctx, admissionIP)
		if aerr := s.Audit(ctx, u.ID, ip, "login_failed", name); aerr != nil {
			return "", Identity{}, aerr
		}
		return "", Identity{}, ErrBadCredentials
	}

	// ADMISSION, AFTER THE CREDENTIAL AND BEFORE ANY SESSION EXISTS.
	//
	// Here rather than in the caller, and here rather than earlier. Earlier
	// would answer a question the caller never earned — whether the name
	// exists — because an unknown name and a known one would fail at
	// different points. Later, in the caller, is where it used to be, and
	// that made a correct password cost a minted-and-revoked session more
	// than an incorrect one. Between the two, nothing has been created yet
	// and the work done is the same as a wrong password's.
	var admittedBy AdmissionSource
	if admissionIP != "" {
		src, aerr := s.IPAllowedForUser(ctx, nil, u.ID, admissionIP)
		if aerr != nil {
			return "", Identity{}, aerr
		}
		if src == NotAdmitted {
			// The audit says what happened; the caller gets the same error
			// a wrong password gets.
			if auerr := s.Audit(ctx, u.ID, ip, "login_failed", name+" (ip not admitted)"); auerr != nil {
				return "", Identity{}, auerr
			}
			return "", Identity{}, ErrBadCredentials
		}
		// CARRIED, NOT WRITTEN. The row goes in the committing transaction
		// below — see there for why.
		admittedBy = src
	}
	if len(u.MKWrapped) == 0 {
		// A v1-era row that never received a keyslot: fail explicitly — an
		// admin passphrase reset cuts one.
		return "", Identity{}, ErrNoKeyslot
	}
	mk, err := open(kek, u.MKWrapped, aadMasterKey)
	if err != nil {
		return "", Identity{}, fmt.Errorf("auth: unwrapping keyslot for %s: %w", name, ErrKeyslotCorrupt)
	}
	// Consistency check, commit, and adoption are ONE critical section
	// Credentials are RE-VERIFIED inside the
	// committing transaction against the current row:
	// a passphrase reset or disable that commits between the
	// out-of-tx verify above and this insert must invalidate this login,
	// otherwise a reset intended to lock someone out races an in-flight
	// old-passphrase session insert.
	verified := u.PassHash
	var token string
	if err := s.withUnlock(mk, func() error {
		return s.inTx(ctx, func(tx *dao.Transaction) error {
			cur, terr := s.store.Users.On(tx).With(meta.UserID, u.ID).Get()
			if terr != nil {
				return terr
			}
			if cur.Disabled != 0 || !bytes.Equal(cur.PassHash, verified) {
				return ErrBadCredentials // credentials changed under us
			}
			if remote != nil {
				token, terr = remote.commitTx(s, tx, cur, ip)
			} else {
				token, terr = s.newSessionTx(tx, u.ID, ip)
			}
			if terr != nil {
				return terr
			}
			if aerr := s.AuditTx(tx, u.ID, ip, "login", name); aerr != nil {
				return aerr
			}
			// THE ADMISSION RECORD COMMITS WITH THE SESSION OR NOT AT ALL.
			//
			// It used to be written the moment the admission check passed,
			// with a comment saying the login had already been decided. It
			// had not: everything between there and here can still fail —
			// a missing keyslot, a master key that will not unwrap, and the
			// recheck just above, which exists precisely because a disable
			// or a passphrase reset can commit underneath an in-flight
			// login. Every one of those left behind a durable row claiming
			// an admitted login that never existed. Review reproduced it
			// deterministically with an empty-keyslot row: no session, and
			// login_admitted went from 0 to 1.
			//
			// An audit failure ROLLS THE LOGIN BACK rather than being
			// dropped. The record of which layer admitted a session is part
			// of what makes an unexpected access recognisable, and a session
			// nobody can account for is worth less than a refused login.
			if admittedBy != "" {
				return s.AuditTx(tx, u.ID, ip, "login_admitted",
					fmt.Sprintf("%s admitted by %s", admissionIP, admittedBy))
			}
			return nil
		})
	}); err != nil {
		return "", Identity{}, err
	}

	return token, Identity{userID: u.ID, name: u.Name, role: u.Role}, nil
}

// decoyAdmission does the admission query's work on a path that has no user
// to ask about, so that path costs what the real one costs.
//
// The sentinel id matches no rows, which is the same query the real check
// runs when a user simply has none. It is skipped entirely when there is no
// admission layer, because then the real path does not run it either — the
// decoy has to mirror what actually happens, not perform unconditional work.
//
// Errors are dropped deliberately: this is equalization, not a decision, and
// a store failure here must not turn a bad-credential refusal into a
// different error that a caller could tell apart.
func (s *Service) decoyAdmission(ctx context.Context, admissionIP string) {
	if admissionIP == "" {
		return
	}
	_, _ = s.IPAllowedForUser(ctx, nil, sentinelUserID, admissionIP)
}

// sentinelUserID belongs to no account. User ids are positive, so a lookup on
// this one does the query and finds nothing.
const sentinelUserID int64 = 0

// Logout revokes the calling session (kept as a row for audit).
func (s *Service) Logout(ctx context.Context, token, ip string) error {
	ident, sess, err := s.resolveToken(ctx, CallerFrom(ctx), token)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *dao.Transaction) error {
		if err := s.store.Sessions.On(tx).With(meta.SessID, sess.ID).
			Set(meta.SessRevoked, int64(1)).Update(); err != nil {
			return err
		}
		return s.AuditTx(tx, ident.userID, ip, "logout", "")
	})
}

// RevokeUserSessions revokes every session of userID (self, or admin).
func (s *Service) RevokeUserSessions(ctx context.Context, token string, userID int64, ip string) error {
	actor, _, err := s.resolveToken(ctx, CallerFrom(ctx), token)
	if err != nil {
		return err
	}
	if actor.userID != userID && actor.role != meta.RoleAdmin {
		return ErrDenied
	}
	return s.inTx(ctx, func(tx *dao.Transaction) error {
		if err := s.revokeAllSessionsTx(tx, userID, 0); err != nil {
			return err
		}
		return s.AuditTx(tx, actor.userID, ip, "sessions_revoked", fmt.Sprintf("user %d", userID))
	})
}

// revokeAllSessionsTx revokes userID's sessions inside tx, sparing
// exceptSessID when non-zero (the caller's own session on passphrase
// change).
func (s *Service) revokeAllSessionsTx(tx *dao.Transaction, userID, exceptSessID int64) error {
	q := s.store.Sessions.On(tx).With(meta.SessUserID, userID)
	if exceptSessID != 0 {
		q = q.Excluding(meta.SessID, exceptSessID)
	}
	return q.Set(meta.SessRevoked, int64(1)).Update()
}

// RequireAdminToken is requireAdmin for callers outside this package.
//
// EXPORTED RELUCTANTLY, AND NARROWLY. The pattern elsewhere is that core owns
// the whole operation — ServiceKeyslotStatusFor authorizes and then answers, so
// the handler cannot be the place the rule is decided. That works when the data
// lives here. The front door's pressure view does not: it is assembled from the
// listener and the engine, neither of which this package should reach into.
//
// So the check is exported rather than the operation, and it returns the
// Identity so a caller that needs to attribute what it does next can. It
// decides one thing — is this token an admin's — and a caller that wants a
// different rule must not build it out of this one.
func (s *Service) RequireAdminToken(ctx context.Context, token string) (Identity, error) {
	return s.requireAdmin(ctx, token)
}
