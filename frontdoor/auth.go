package frontdoor

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// Authentication and session open (protocol matrix rows 2.6-2.9).
//
// One credential exchange, one verification, one answer. The chain that
// decides is row 2.7's, and it lives in the engine — this file is the wire
// around it: offer the method, read the frame, hand the token over, and turn
// whatever comes back into either a session or the one uniform denial.

// AuthDeadline bounds the credential exchange (§9: startup/auth 10s).
//
// Its own deadline rather than a share of the startup budget, for the reason
// every phase here has its own: a peer that spent the whole allowance getting
// through TLS must not still be owed time to sit holding an open socket while
// deciding whether to send a password.
const AuthDeadline = 10 * time.Second

// IdleDeadline is what replaces the pre-auth deadlines once a session is
// open (§9: between-messages, 30m idle).
//
// Re-arming is not bookkeeping. The pre-auth deadlines are ten seconds, and a
// deadline set on a net.Conn STAYS SET — leaving one in place would mean an
// authenticated session died ten seconds after it opened, and the first
// person to notice would be a developer sitting on a debug breakpoint
// wondering why their connection dropped. The >90s cell exists for exactly
// that reader.
const IdleDeadline = 30 * time.Minute

// CancelKeyLen is the BackendKeyData secret's length in bytes.
//
// Four, because we negotiate every client down to protocol 3.0 (row 2.5) and
// 3.0's cancel key is a fixed int32. pgproto3 models the field as a []byte to
// carry 3.2's variable-length key, so nothing in the library would stop us
// sending sixteen bytes to a 3.0 client that will read four and lose the
// frame boundary.
const CancelKeyLen = 4

// Authenticator is the front door's view of the engine.
//
// An interface rather than a *exec.Engine so this package can be exercised
// against a fake without a meta store — and, more to the point, so the
// listener's dependency is exactly the calls it makes. A concrete engine
// here would let a later change reach for anything on it.
type Authenticator interface {
	OpenWireSession(ctx context.Context, presented, startupUser, database, ip string) (exec.WireSessionResult, error)
	CloseWireSession(ctx context.Context, id exec.SessionID, userID int64, ip, reason string)
}

// CancelExecutor is the engine's cancel-registry half (§6.4), as seen by the
// listener.
//
// The three calls are the whole of row 2.3's engine surface: register the
// pair at session open so the key a client holds can be honoured, forget it
// when the session ends so it cannot point at whatever later takes the same
// process id, and resolve a presented pair — constant-time, statement-only —
// when a cancel connection arrives.
//
// An interface for the same reason Authenticator is one: a cell can drive the
// listener against a fake, and the seam documents exactly which engine calls
// this slice makes. Implementations must be safe for concurrent use: cancel
// connections arrive on their own goroutines, unrelated to the session they
// name.
type CancelExecutor interface {
	// RegisterCancelKey records a freshly minted BackendKeyData pair against
	// the session that will receive it. Called BEFORE the key is sent, so a
	// client can never hold a key the engine has forgotten — the inverse
	// window is harmless: a registered key for a session the client has not
	// seen yet cancels nothing, because CancelByKey resolves through the
	// live session.
	RegisterCancelKey(id exec.SessionID, userID int64, key exec.CancelKey) error
	// RevokeCancelKey forgets a session's pair.
	RevokeCancelKey(id exec.SessionID)
	// CancelByKey stops the statement the pair names, if it matches a live
	// session. The bool is for the fd.cancel_applied / fd.cancel_stale audit
	// split and must never reach the wire.
	CancelByKey(ctx context.Context, key exec.CancelKey) bool
}

// authOutcome is what the credential exchange produced. Exactly one of
// Session and Denied is meaningful.
type authOutcome struct {
	Session exec.WireSessionResult
	Denied  denialReason
	// Counts reports whether this denial is the PEER's fault and should be
	// charged to their source address. A store failure is not.
	Counts bool
	// Peer reports the same thing for a non-denial ERROR return: a read
	// failure is the peer's doing, while running out of workers or a stuck
	// store is ours. Separate from Counts because one accompanies a denial
	// and the other accompanies an error, and collapsing them would make the
	// caller guess which it had.
	Peer bool
}

// runAuth performs rows 2.6-2.8 on an established TLS connection.
//
// It reads AT MOST ONE credential frame. The matrix cell said three attempts
// per connection and rev 6 amends it to one, because re-prompting is not a
// defence: libpq answers a repeated AuthenticationCleartextPassword with the
// same password it already sent, so a ceiling of three would spend three PAT
// verifications on one wrong password. That is amplification pointed at
// ourselves. PostgreSQL closes on the first failure and so do we; the throttle
// that actually bounds grinding is the per-source-address one, which survives
// the reconnect that a per-connection ceiling does not.
func (l *Listener) runAuth(ctx context.Context, conn net.Conn, be *pgproto3.Backend, params map[string]string, peer string) (authOutcome, error) {
	if l.authn == nil {
		// The honest state of a build with no engine behind the listener.
		// Recorded as its own reason so it is never mistaken in the trail
		// for a credential that was checked and found wanting.
		return authOutcome{Denied: reasonNoCredentialStore}, nil
	}

	// ONE DEADLINE FOR THE WHOLE CREDENTIAL EXCHANGE, absolute, taken here.
	//
	// It was three budgets that ADDED UP. The socket got l.dl.auth before the
	// read; the worker wait then started a FRESH timer of the same length;
	// and the verification itself got the listener's context, which has no
	// deadline at all. A peer could spend twice the budget by being slow at
	// the right moment — lector measured 502.96ms against a 300ms setting —
	// and a stuck auth store could hold a worker forever, because nothing
	// upstream was ever going to cancel it.
	//
	// The phase's budget is a property of the PHASE, not of each step in it.
	// Every step below shares this instant: the socket, the queue, and the
	// store call.
	authDeadline := l.now().Add(l.dl.auth)
	authCtx, cancelAuth := context.WithDeadline(ctx, authDeadline)
	defer cancelAuth()

	// Row 2.6: cleartext, and it is the ONLY method offered. SCRAM cannot be
	// offered over hashed PATs — a SCRAM verifier needs material the server
	// deliberately does not keep — so offering it would be a menu item that
	// fails for everyone who picks it.
	be.Send(&pgproto3.AuthenticationCleartextPassword{})
	if err := be.Flush(); err != nil {
		return authOutcome{}, err
	}
	// Row 2.8: after this, EVERY type-`p` frame decodes as a PasswordMessage,
	// SASL- and GSS-shaped bytes included. There is no distinguishable SASL
	// path to leak, because by the protocol there is no SASL path at all
	// once cleartext is what was offered.
	//
	// NO TEST CAN OBSERVE THIS CALL, and that is worth stating rather than
	// leaving for someone to discover by deleting it. pgproto3 decodes an
	// unset auth type as a PasswordMessage anyway, in a branch its own source
	// labels "to maintain backwards compatibility" — so removing this line
	// changes nothing today. It is here because that is a fallback the
	// library has told us is a fallback, and a surface whose SASL-shaped
	// frames must not take a SASL path should say which decode it wants
	// rather than inherit one that exists to avoid breaking old callers.
	if err := be.SetAuthType(pgproto3.AuthTypeCleartextPassword); err != nil {
		return authOutcome{}, err
	}

	if err := conn.SetDeadline(authDeadline); err != nil {
		return authOutcome{}, err
	}
	msg, err := be.Receive()
	if err != nil {
		// A read failure is not a denial: nothing was presented. It closes
		// without a frame for the same reason a TLS failure does, and it IS
		// the peer's doing, so it is charged to them.
		return authOutcome{Peer: true}, err
	}
	pm, ok := msg.(*pgproto3.PasswordMessage)
	if !ok {
		// A frame that is not type-`p` before authentication — a Query, a
		// Parse. Unambiguous protocol violation, and the peer's fault, so it
		// is charged to their address like any other failed attempt.
		return authOutcome{Denied: reasonPreAuthProtocolViolation, Counts: true}, nil
	}

	// THE WORKER GATE (matrix §9), taken here and not earlier.
	//
	// Around the VERIFICATION rather than around the whole credential phase,
	// and the difference is availability. A slot held from the moment the
	// prompt is offered would be held while a client decides whether to
	// answer — so sixteen silent peers, costing nothing to run, would keep
	// every real client out for the whole auth deadline. Held from the frame
	// arriving, it bounds exactly the work that is expensive and nothing a
	// peer can stretch for free.
	release, werr := l.acquireAuthWorker(authCtx)
	if werr != nil {
		// OUR CAPACITY, NOT THEIR CREDENTIAL. A peer who waited for a worker
		// we could not spare presented something we never looked at, and
		// charging that to their address would throttle them for our
		// shortfall — the same distinction the store-failure path already
		// draws. Peer stays false, so nothing is counted against them.
		return authOutcome{}, werr
	}
	res, aerr := l.authn.OpenWireSession(authCtx, pm.Password, params["user"], params["database"], hostOf(peer))
	release()
	if aerr != nil {
		if reason := exec.DenialReason(aerr); reason != "" {
			return authOutcome{Denied: denialReason(reason), Counts: true}, nil
		}
		// A store failure. The wire still gets the uniform denial — telling
		// a caller that our database is unreachable is an answer they have
		// not earned either — but the audit says what it was, and the
		// address is NOT charged for our outage.
		l.onLog(fmt.Sprintf("frontdoor: authenticating %s: %v", peer, aerr))
		return authOutcome{Denied: reasonAuthStoreError}, nil
	}
	return authOutcome{Session: res}, nil
}

// acquireAuthWorker takes one of the credential-phase slots, or gives up when
// the connection's own deadline says it should.
//
// Bounded by the SAME auth deadline the read was, so a queue cannot become a
// way to hold connections open past their budget: a peer that waits for a
// worker is spending its own ten seconds, not new ones. A caller that gives
// up here is closed without a denial frame — it presented a credential we
// never looked at, and saying "authentication failed" would be a claim about
// a check that did not happen.
func (l *Listener) acquireAuthWorker(ctx context.Context) (func(), error) {
	if l.authSlots == nil {
		return func() {}, nil
	}
	// SELECTS ON THE PHASE'S CONTEXT, and starts no timer of its own. A
	// fresh duration here was a second full budget: a peer who had already
	// spent the whole allowance getting to this line could then wait the
	// whole allowance again.
	select {
	case l.authSlots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-l.authSlots }) }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("frontdoor: no credential worker within the authentication "+
			"deadline: %w", ctx.Err())
	}
}

// completeHandshake is row 2.9: the success sequence, in the protocol's order.
//
// Nothing here acquires anything. Every slot, the lease and the memory charge
// were taken atomically inside row 2.7, which is what makes it true that a
// client seeing ReadyForQuery is a client whose capacity is already held —
// there is no window between "you are in" and "and there was room".
//
// Row 2.3 makes the BackendKeyData pair real: the same mint that composes
// the frame REGISTERS the pair, so the key a client receives is a key the
// engine can honour. Registered BEFORE the frame is sent — the inverse window
// (registered, not yet sent) is harmless because a key resolves through a
// live session, while the forward window (sent, not yet registered) is a
// client holding a capability the server would refuse.
//
// A COLLISION REMINTS RATHER THAN REDRAWS (PR #44 r0). If the minted process
// id is already held by another session, the engine refuses with
// ErrCancelKeyCollision and this loop mints a FRESH pair and registers that —
// the pid the client receives and the pid the registry holds are one object,
// because both come from the same `key`. Redrawing inside the registration
// seam would have sent the client one pid and recorded another. With a
// 32-bit space and one redraw budget, an eight-round failure is a broken
// CSPRNG and the handshake fails rather than sends an unhonourable key.
func (l *Listener) completeHandshake(be *pgproto3.Backend, res exec.WireSessionResult, params map[string]string, notes []paramNote) error {
	be.Send(&pgproto3.AuthenticationOk{})
	// §3.1: an over-long application_name earns a NoticeResponse. The notice
	// names the cap and the fact, never the original value — that went to the
	// audit, and echoing it here would defeat the cap.
	for _, n := range notes {
		if n.Kind == noteApplicationNameTruncated {
			be.Send(&pgproto3.NoticeResponse{
				Severity: "NOTICE", SeverityUnlocalized: "NOTICE", Code: "01000",
				Message: "application_name was longer than 256 bytes and was truncated",
			})
		}
	}
	for _, ps := range synthesizedStatuses(res, params) {
		be.Send(ps)
	}
	var key *pgproto3.BackendKeyData
	for range 8 {
		minted, err := newBackendKey()
		if err != nil {
			return err
		}
		registered := exec.CancelKey{ProcessID: minted.ProcessID}
		copy(registered.Secret[:], minted.SecretKey)
		if l.cancels == nil {
			key = minted // the honest degraded state: every cancel lands stale
			break
		}
		if err := l.cancels.RegisterCancelKey(res.SessionID, res.UserID, registered); err == nil {
			key = minted
			break
		} else if !errors.Is(err, exec.ErrCancelKeyCollision) {
			return fmt.Errorf("frontdoor: registering the cancel key: %w", err)
		}
	}
	if key == nil {
		return errors.New("frontdoor: could not mint an uncollided cancel key")
	}
	be.Send(key)
	be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	return be.Flush()
}

// synthesizedStatuses is §3.3's three overridden values.
//
// ONLY the three. §3.3 requires the target connection's own reported set to
// be forwarded VERBATIM ahead of these, and that set does not exist until a
// lease is held — which is F1's slice. Sending a plausible fixed list in the
// meantime is precisely what §3.3 forbids and would be worse than sending
// nothing, because a client would believe it had been told the server's
// DateStyle. The matrix cell records the split (rev 6).
func synthesizedStatuses(res exec.WireSessionResult, params map[string]string) []pgproto3.BackendMessage {
	return []pgproto3.BackendMessage{
		// The echo of §3.1's accepted application_name. Absent is a legal
		// startup, and an empty echo is the honest answer to it.
		&pgproto3.ParameterStatus{Name: "application_name", Value: params["application_name"]},
		// ALWAYS off. A client asking whether it is superuser is asking a
		// question about the TARGET's role, and the answer through this
		// surface is that autodb's gates apply regardless of what the target
		// would have said.
		&pgproto3.ParameterStatus{Name: "is_superuser", Value: "off"},
		// The autodb username, canonical rather than as typed.
		&pgproto3.ParameterStatus{Name: "session_authorization", Value: res.UserName},
	}
}

// newBackendKey mints the cancel key from the CSPRNG (matrix row 2.9, MF7).
//
// From crypto/rand and nowhere else. A cancel key IS a capability: whoever
// holds it can cancel that session's running statement, so a key drawn from a
// process-id-and-timestamp scheme — the shape this kind of code drifts into —
// is one a stranger can guess and use.
func newBackendKey() (*pgproto3.BackendKeyData, error) {
	var b [4 + CancelKeyLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, fmt.Errorf("frontdoor: cancel key: %w", err)
	}
	return &pgproto3.BackendKeyData{
		ProcessID: binary.BigEndian.Uint32(b[0:4]),
		SecretKey: b[4:],
	}, nil
}
