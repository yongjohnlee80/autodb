package frontdoor

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/outcome"
)

// Authentication and session open (protocol matrix rows 2.6-2.9).
//
// One credential exchange, one verification, one answer. The chain that
// decides is row 2.7's, and it lives in the engine — this file is the wire
// around it: offer the method, read the frame, hand the token over, and turn
// whatever comes back into either a session or the one uniform denial.

// AuthDeadline bounds the credential exchange (matrix §9: startup/auth 10s).
//
// Its own deadline rather than a share of the startup budget, for the reason
// every phase here has its own: a peer that spent the whole allowance getting
// through TLS must not still be owed time to sit holding an open socket while
// deciding whether to send a password.
const AuthDeadline = 10 * time.Second

// IdleDeadline is what replaces the pre-auth deadlines once a session is
// open (matrix §9: between-messages, 30m idle).
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
	// OpenWireSessionWith is the call this listener makes (#58's seam). It
	// carries the startup parameters the session must remember — today the
	// accepted application_name, which row 3.1 requires on the session and on
	// every audit row for a wire unit (claim #session-audit).
	OpenWireSessionWith(ctx context.Context, req exec.WireOpen) (exec.WireSessionResult, error)
	// OpenWireSession is kept because it is what the fakes implement and what
	// the engine still exposes; it opens with no application_name.
	OpenWireSession(ctx context.Context, presented, startupUser, database, ip string) (exec.WireSessionResult, error)
	CloseWireSession(ctx context.Context, id exec.SessionID, userID int64, ip, reason string)
}

// CancelExecutor is the engine's cancel-registry half (matrix §6.4), as seen by the
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
	// Failure names an ERROR-DRIVEN ending -- a read that broke, a worker we
	// could not spare, a store that would not answer -- so the charge for it
	// comes from the registry like every other outcome's.
	//
	// Respond says what the peer is told, and it is carried rather than
	// inferred: an error-driven ending may still owe the caller the uniform
	// denial, which is why a store outage does not have to become a "refusal"
	// in order to write bytes.
	//
	// IT REPLACES TWO BOOLEANS. Counts and Peer said whether to charge, beside
	// a registry that also said whether to charge, and the two could disagree:
	// all three failure paths shared one identity registered as never-charged
	// while a Boolean charged one of them. One authority, and it is the
	// registered class.
	Failure outcome.ReasonID
	// Detail is the safe diagnostic for an error-driven ending: the closed-set
	// literal, never a formatted cause. Carried so the startup endings can put
	// something actionable in the trail without the audit row being the place
	// a secret escapes.
	Detail  string
	Respond WireResponse
	// Disclosable carries the engine's witness that this refusal happened
	// AFTER the credential verified, which is the only condition under which
	// the wire may say what went wrong. Not derived from the reason: see
	// exec.DenialDisclosable.
	Disclosable bool
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
func (l *Listener) runAuth(ctx context.Context, conn net.Conn, be *pgproto3.Backend, fr *frameReader,
	params map[string]string, gucs map[string]string, peer string) (authOutcome, error) {
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
	// the right moment — review measured 502.96ms against a 300ms setting —
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
		// OURS: the prompt could not be written. The peer has not been asked
		// for anything yet, so there is nothing they could have done wrong,
		// and the socket is in no state to carry an answer.
		return authOutcome{Failure: outcomeID(OutcomeAuthSetupFailed)}, err
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
		return authOutcome{Failure: outcomeID(OutcomeAuthSetupFailed)}, err
	}

	if err := conn.SetDeadline(authDeadline); err != nil {
		return authOutcome{Failure: outcomeID(OutcomeAuthSetupFailed)}, err
	}
	msg, err := be.Receive()
	// The queue advances for auth's own frames too: it is shared with the session
	// loop, and a Receive that does not pop leaves every later header attributed
	// to the wrong frame.
	if fr != nil {
		fr.consumeHeader()
	}
	if err != nil {
		// A read failure is not a denial: nothing was presented. It closes
		// without a frame for the same reason a TLS failure does, and it IS
		// the peer's doing, so it is charged to them.
		// THEIRS: the peer went away, or never answered. Charged, and the
		// registry is what says so.
		return authOutcome{Failure: outcomeID(OutcomeAuthReadFailed)}, err
	}
	pm, ok := msg.(*pgproto3.PasswordMessage)
	if !ok {
		// A frame that is not type-`p` before authentication — a Query, a
		// Parse. Unambiguous protocol violation, and the peer's fault, so it
		// is charged to their address like any other failed attempt.
		return authOutcome{Denied: reasonPreAuthProtocolViolation}, nil
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
		// draws. Its registered class is None, so nothing is counted.
		return authOutcome{Failure: outcomeID(OutcomeAuthWorkerBusy)}, werr
	}
	res, aerr := l.authn.OpenWireSessionWith(authCtx, exec.WireOpen{
		PAT: pm.Password, StartupUser: params["user"], Database: params["database"],
		IP: hostOf(peer),
		// The listener knows its own transport; the engine must not have to
		// re-derive it from configuration it does not hold.
		Cleartext: l.cleartextDebug,
		// Already length-capped and audited verbatim by params.go — the cap is
		// row 3.1's, and applying it before the engine sees the label keeps the
		// one rule in one place.
		ApplicationName: params["application_name"],
		// The amended rule: the settings matrix §3.1 collected, judged by the engine's own
		// denylist — the same one a SET from this session would meet. The front
		// door does not decide which are allowed; deciding here would be a
		// second opinion about a rule that lives in one place on purpose.
		StartupGUCs: gucs,
	})
	release()
	if aerr != nil {
		if reason := exec.DenialReason(aerr); reason != "" {
			return authOutcome{
				Denied:      denialReason(reason),
				Disclosable: exec.DenialDisclosable(aerr),
			}, nil
		}
		// A LOCKED STORE DOES ARRIVE HERE, AND THIS PACKAGE SAID IT COULD NOT.
		//
		// The claim that stood here was that OpenWireSessionWith never opens a
		// target, so the DSN is decrypted at the first statement and a locked
		// store lets a client authenticate and refuses its query. That is true
		// of connections whose engine does not speak the PostgreSQL wire. It
		// is FALSE of the ones this front door is for: OpenWireSessionWith
		// pins the backend for a PostgreSQL-wire connection before the client
		// is ever told the session is ready, so decrypting the DSN -- and
		// resolving the target, and asserting the driver's capabilities --
		// all happen INSIDE the credential phase.
		//
		// The cell that was supposed to guard the claim used a SQLite fixture,
		// which never reaches the pin, so it proved the claim on the one
		// engine the claim happens to hold for and said nothing about the
		// others. A guard that cannot fail on the case it guards is not a
		// guard; correcting that cell is part of this change.
		//
		// WHAT THE OLD CODE DID WITH IT. Everything that was not a denial
		// became an auth-store error answered with the uniform credential
		// denial, so a developer holding a verified token, against a
		// connection whose secret store was locked, was told their CREDENTIAL
		// was wrong.
		//
		// WHETHER THOSE RETRIES WERE ALSO CHARGED IS NOT ASSERTED HERE. An
		// earlier version of this comment said they were, and that claim was
		// never traced to a commit or to a charging occurrence -- the
		// auth-store identity is registered ChargeNone, so the charge would
		// have had to come from somewhere else, and nobody established where.
		// What IS established, and is what this branch is for, is the wrong
		// answer above. The throttling behaviour of the identity now selected
		// is proved by the repetition cell in locked_store_test.go rather
		// than asserted in prose.
		//
		// So the two are branched before the generic arm, each with its own
		// registered identity and its own safe FATAL frame. The caller learns
		// that the connection is unavailable or misconfigured, which is true,
		// costs them no failure budget, and tells them nothing about why.
		if startup, detail := startupFailure(aerr); startup != "" {
			// THE SAFE DIAGNOSTIC, AND NOT THE CAUSE, EVEN HERE.
			//
			// This used to log "%v" of the error, on the reasoning that a log
			// is the operator's and the wire is the caller's. That reasoning
			// does not survive contact with what a log actually is: it is
			// copied into tickets, pasted into chat and shipped to
			// aggregators. Measured, the cause for a DSN failure carried the
			// target host, a password passed as a query parameter, and a
			// PAT-shaped token in the username position -- pgx redacts the
			// userinfo password and nothing else. The safe diagnostic names
			// the stage, the connection's opaque id and which check failed,
			// which is what an operator needs to find the row.
			l.onLog(fmt.Sprintf("frontdoor: no wire session for %s: %s", peer, detail))
			return authOutcome{Failure: startup, Detail: detail, Respond: WireStartupFatal}, aerr
		}
		// A store failure. The wire still gets the uniform denial — telling
		// a caller that our database is unreachable is an answer they have
		// not earned either — but the audit says what it was, and the
		// address is NOT charged for our outage.
		// THE SAME RULE FOR THE GENERIC ARM. What reaches here is whatever
		// the engine returned, and this package cannot know what is in it, so
		// it says which phase failed and nothing else. An operator with the
		// peer and the timestamp can find the engine's own record.
		l.onLog(fmt.Sprintf("frontdoor: authenticating %s: the credential store did not "+
			"complete the exchange", peer))
		// OURS, AND ERROR-DRIVEN, AND STILL OWED AN ANSWER. Three separate
		// facts, carried separately: the identity is operational, the charge
		// is none, and the peer gets the uniform denial because telling them
		// our database is unreachable is an answer they have not earned
		// either.
		//
		// It was briefly declared a refusal purely because it writes bytes,
		// which conflated the wire with the kind and filed a store outage
		// under the number an operator watches for credential attacks.
		return authOutcome{Failure: outcomeID(string(reasonAuthStoreError)),
			Respond: WireUniformDenial}, aerr
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
// A COLLISION REMINTS RATHER THAN REDRAWS. If the minted process
// id is already held by another session, the engine refuses with
// ErrCancelKeyCollision and this loop mints a FRESH pair and registers that —
// the pid the client receives and the pid the registry holds are one object,
// because both come from the same `key`. Redrawing inside the registration
// seam would have sent the client one pid and recorded another. With a
// 32-bit space and one redraw budget, an eight-round failure is a broken
// CSPRNG and the handshake fails rather than sends an unhonourable key.
func (l *Listener) completeHandshake(be *pgproto3.Backend, res exec.WireSessionResult, params map[string]string, notes []paramNote) error {
	be.Send(&pgproto3.AuthenticationOk{})
	// matrix §3.1: an over-long application_name earns a NoticeResponse. The notice
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
	// FORWARD THE TARGET'S OWN SET FIRST, VERBATIM (row 3.3). These are what the
	// pinned backend actually reported at its startup — server_version,
	// client_encoding, DateStyle, TimeZone and the rest — and forwarding them is
	// what makes the relay honest: a client that reads server_version through
	// this surface learns the TARGET's, because that is the server its
	// statements will run on.
	//
	// Before #58's seam the front door had no way to know them, so it sent only
	// the three it could synthesize and a client asking for DateStyle was told
	// nothing at all. Row 3.3's forwarded half was `awaiting` for exactly that
	// reason.
	//
	// Order matters and is deliberate: forwarded first, overrides after. A
	// ParameterStatus later in the stream is the one the client keeps, so the
	// two below win over anything the target reported under the same name —
	// which is the point of overriding them rather than filtering the set.
	out := make([]pgproto3.BackendMessage, 0, len(res.ParameterStatuses)+3)
	for _, name := range sortedNames(res.ParameterStatuses) {
		out = append(out, &pgproto3.ParameterStatus{Name: name, Value: res.ParameterStatuses[name]})
	}

	return append(out,
		// The echo of matrix §3.1's accepted application_name — the CLIENT's own label
		// coming back to it, now taken from what the ENGINE accepted rather than
		// re-read from the startup params, so the echo cannot disagree with what
		// the session recorded and audits under (claim #session-audit).
		//
		// The target is never sent this label at startup, so its own set will
		// carry the backend's effective default; the override is what makes the
		// echo true. (A client can still change the backend's GUC later through
		// set_config(); see params.go.)
		&pgproto3.ParameterStatus{Name: "application_name", Value: res.ApplicationName},
		// ALWAYS off, whatever the target reported. A client asking whether it is
		// superuser is asking a question about the TARGET's role, and the answer
		// through this surface is that autodb's gates apply regardless.
		&pgproto3.ParameterStatus{Name: "is_superuser", Value: "off"},
		// The autodb username, canonical rather than as typed.
		&pgproto3.ParameterStatus{Name: "session_authorization", Value: res.UserName},
	)
}

// sortedNames orders the forwarded set so the stream is deterministic.
//
// PostgreSQL does not promise an order and a client must not depend on one, but
// a cell comparing two connections' sets would otherwise fail on map iteration
// rather than on a difference that matters.
func sortedNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// newBackendKey mints the cancel key from the CSPRNG.
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

// startupFailure names the registered outcome for a wire-session open that
// failed for one of OUR reasons, with the fixed literal that may be published
// about it, or "" when the failure is not one of them.
//
// IT LISTS RATHER THAN INFERS, for the reason the acquisition path lists its
// own answers: there is nothing in a wrapped error that distinguishes "the
// secret store is locked" from "the target refused our credential", and
// guessing would put a wrong fact in an operator's trail. A failure that is
// not listed keeps the generic store-outage handling, which is the
// conservative half.
//
// THE DETAIL COMES FROM THE CLOSED SET, never from the error. A configuration
// failure already carries the literal its raise site chose; a locked store has
// no such carrier, so the one member of the set that describes it is named
// here.
func startupFailure(err error) (outcome.ReasonID, string) {
	if c, ok := exec.ConfigFailureOf(err); ok {
		return outcomeID(OutcomeStartupConnectionUnusable), c.AuditDetail()
	}
	if errors.Is(err, auth.ErrLocked) {
		return outcomeID(OutcomeStartupConnectionUnavailable), string(exec.DetailStoreUnavailable)
	}
	return "", ""
}
