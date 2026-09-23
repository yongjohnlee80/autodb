package rpc

import (
	"context"
	"encoding"
	"errors"
	"fmt"
	"github.com/yongjohnlee80/autodb/core/pressure"
	"math"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/yongjohnlee80/autodb/core/admission"
	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/engine"
	"github.com/yongjohnlee80/autodb/core/exec"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
)

// autodb wire error codes, alongside the transport's (-32601..-32001).
//
//	[Internal Core Error Raised]
//	               │
//	               ▼
//	      wireErr() Inspection
//	               │
//	      Is in publicErrs?
//	      ┌────────┴────────┐
//	     YES               NO
//	      │                 │
//	      ▼                 ▼
//	[Public Code]     [Opaque Wire Error]
//	Return mapped     Return -32603 Internal Error
//	negative code     ("internal error")
const (
	// CodeHandshakeRequired gates methods before a compatible sys.hello.
	CodeHandshakeRequired int64 = -32021
	// CodeProtocolMismatch refuses an incompatible client (re-provision).
	CodeProtocolMismatch int64 = -32020
	// CodeAuth carries credential/session failures (bad login, stale token,
	// locked store, policy refusals like weak passphrase or last admin).
	CodeAuth int64 = -32030
	// CodeDenied carries authorization denials (role/grant/IP).
	CodeDenied int64 = -32031
	// CodeStatementRejected carries the execution gate's refusals
	// (classification, WHERE-less guard, size cap).
	CodeStatementRejected int64 = -32032

	// The ExecSession family. Protocol 5 added
	// the session verbs but no codes for what they can refuse, so every
	// session error — no such session, already running, cap reached —
	// fell through wireErr as an unmapped error and reached the client as a
	// generic internal failure. A client cannot act on that: "the server
	// broke" and "you already have eight sessions open" call for opposite
	// responses, and only one of them is worth retrying.
	//
	// The split is by what the CLIENT should do, which is the only thing a
	// code is for:

	// CodeSessionNotFound: the session does not exist, or is not this
	// caller's. Deliberately one code for both — the id space must not
	// become a way to discover which sessions exist. Reopen.
	CodeSessionNotFound int64 = -32040
	// CodeSessionBusy: one in-flight statement per session, and this
	// session already has one. Wait for the previous call, or use another
	// session; the request was not run.
	CodeSessionBusy int64 = -32041
	// CodeSessionCapExceeded: the per-user or global session cap is full.
	// Close a session, or wait for one to be reaped. Retrying immediately
	// will fail the same way.
	CodeSessionCapExceeded int64 = -32042
	// CodeTxState: the request is wrong for the transaction's CURRENT state
	// — no transaction open, one already open, or an aborted transaction
	// that accepts only ROLLBACK. The fix is a different statement, not a
	// retry of this one.
	CodeTxState int64 = -32043
	// CodeConnectionDraining: the connection is being deleted or shut down.
	// Nothing on it will succeed again; this is not a retry.
	CodeConnectionDraining int64 = -32044
	// CodeNoSuchTx: tx.status was asked about a transaction id with no
	// record — or one belonging to someone else, which is deliberately
	// indistinguishable. Not a pending status: a mistyped or expired id must
	// not leave a caller polling forever for a transaction that never was.
	CodeNoSuchTx int64 = -32045
	// CodeInvalidToken carries PAT MANAGEMENT refusals — a duplicate name, a
	// cap reached, a lifetime out of range, an allowed_ips that is not a
	// subset. The caller is authenticated and managing their own tokens, so
	// the specific reason is theirs to see.
	//
	// It is emphatically NOT the front door's credential failure. That one
	// is uniform and anonymous by design, and it never travels this wire.
	CodeInvalidToken int64 = -32046

	// CodeKeyslot reports a SERVICE KEYSLOT mutation an admin can fix:
	// no keyfile path configured, a slot that already exists,
	// no slot to remove, a keyfile with unsafe permissions, a malformed one.
	//
	// Its own code rather than a shared "invalid argument", because these have
	// DIFFERENT REMEDIES and a caller that cannot tell them apart sends its
	// operator to the wrong file. The caller is an authenticated admin acting
	// on their own install, so the sentinel text is theirs to see — this is
	// nothing like the front door's uniform credential denial, which is
	// anonymous by design and never travels this wire.
	CodeKeyslot int64 = -32047

	// CodeDialFailed and CodeConfigFailed carry the two TYPED backend-acquisition
	// failures onto the RPC wire. They exist because a dial that could not be
	// established and a connection that cannot be used as configured have
	// DIFFERENT REMEDIES — whitelist an IP / check the network, versus fix the
	// connection's configuration — and a caller that cannot tell them apart is
	// sent to the wrong file, exactly the split the codes above encode. What a
	// caller reads in the Message depends on the SURFACE (see the wireErr
	// method); the code, and therefore the remedy, does not.
	CodeDialFailed   int64 = -32048
	CodeConfigFailed int64 = -32049
)

// publicErrs is the whole disclosure allowlist: core sentinels whose
// CONSTANT text is deliberately public, each with its wire code. Order
// matters only in that ErrDenied precedes the CodeAuth family.
var publicErrs = []struct {
	sentinel error
	code     int64
}{
	{auth.ErrDenied, CodeDenied},
	{auth.ErrBadCredentials, CodeAuth},
	{auth.ErrTokenInvalid, CodeAuth},
	{auth.ErrLocked, CodeAuth},
	{auth.ErrWeakPassphrase, CodeAuth},
	{auth.ErrBootstrapDone, CodeAuth},
	{auth.ErrLastAdmin, CodeAuth},
	{auth.ErrNoKeyslot, CodeAuth},
	{exec.ErrEmptyStatement, CodeStatementRejected},
	{exec.ErrMultiStatement, CodeStatementRejected},
	{exec.ErrStatementUnsupported, CodeStatementRejected},
	{exec.ErrMalformedStatement, CodeStatementRejected},
	{exec.ErrNoWhere, CodeStatementRejected},
	{exec.ErrScriptTooLarge, CodeStatementRejected},
	{exec.ErrReaderAdvancedPattern, CodeStatementRejected},
	{exec.ErrWireSetRefused, CodeStatementRejected},
	{exec.ErrReadOnlyUnenforceable, CodeStatementRejected},
	{exec.ErrGrammarDrifted, CodeStatementRejected},
	{exec.ErrConnectionNameTaken, golibrpc.CodeInvalidParams},
	{exec.ErrConnectionHasHistory, golibrpc.CodeInvalidParams},
	// Workspace not-found is admin-only reachable (Manage authz runs
	// BEFORE the lookup, so R13 ordering holds) and carries no internals.
	{exec.ErrWorkspaceNotFound, golibrpc.CodeInvalidParams},

	// The session/transaction surface (protocol 5). Each of these is a
	// condition the CALLER can do something about, which is why they are
	// public: their constant text says what happened and the code says what
	// to do about it. None of them names a connection, a user, or another
	// session, so publishing them discloses nothing about what exists.
	{exec.ErrSessionNotFound, CodeSessionNotFound},
	// A transaction that is not the caller's answers exactly as one that
	// never existed — same sentinel, same code, same text — so tx.status
	// cannot be used to discover which transaction ids exist.
	{exec.ErrNoSuchTx, CodeNoSuchTx},

	// PAT management failures. These reach an AUTHENTICATED caller managing
	// their own tokens, so naming them is safe and useful — unlike
	// auth.ErrPATInvalid, which is the anonymous front-door path's single
	// uniform failure and is deliberately absent from this list.
	{auth.ErrPATNameTaken, CodeInvalidToken},
	{auth.ErrPATCapExceeded, CodeInvalidToken},
	{auth.ErrPATBadExpiry, CodeInvalidToken},
	{auth.ErrPATBadAllowedIPs, CodeInvalidToken},
	{auth.ErrPATNotFound, CodeInvalidToken},
	// The mint gates. Both MUST be mapped: an unmapped sentinel
	// reaches the caller as a -32603 internal fault, which would tell someone
	// who named the wrong connection that the SERVER broke — the same reason
	// ErrPATNotFound is on this list.
	//
	// ErrPATConnDenied deliberately covers "no such connection" AND "not
	// yours" with one code and one text, so the pair cannot be told apart
	// from the wire (R13). ErrPATConnNotFrontDoor is post-grant and names the
	// connection on purpose: it is the one refusal that has to be actionable.
	{auth.ErrPATConnDenied, CodeInvalidToken},
	{auth.ErrPATConnNotFrontDoor, CodeInvalidToken},
	{auth.ErrPATDebugCleartextRefused, CodeInvalidToken},
	// Keyslot mutations. Denied is the admin gate; the rest are operator
	// mistakes with distinct remedies, so they must not collapse into one
	// code — "no keyfile path configured" and "a slot already exists" send an
	// operator to different places.
	{auth.ErrNoKeyfilePath, CodeKeyslot},
	{auth.ErrServiceKeyslotExists, CodeKeyslot},
	{auth.ErrNoServiceKeyslot, CodeKeyslot},
	{auth.ErrKeyfileMode, CodeKeyslot},
	{auth.ErrKeyfileMalformed, CodeKeyslot},
	{exec.ErrSessionBusy, CodeSessionBusy},
	{exec.ErrSessionCapExceeded, CodeSessionCapExceeded},
	{exec.ErrConnectionDraining, CodeConnectionDraining},

	// Transaction-state refusals all map to one code: the caller's next
	// move is the same in every case — send a different statement, not this
	// one again — and the sentinel's own text says which state it was in.
	{exec.ErrTxAlreadyOpen, CodeTxState},
	{exec.ErrNoOpenTx, CodeTxState},
	{exec.ErrTxAborted, CodeTxState},
	{exec.ErrTxChainUnsupported, CodeTxState},

	// The session-state gate (SET / LOCK). These are refusals of a specific
	// statement, like the classification gate above, so they carry its code
	// rather than a new one — a caller treats them the same way.
	{exec.ErrSetNotLocal, CodeStatementRejected},
	{exec.ErrSetGUCRefused, CodeStatementRejected},
	{exec.ErrSetOutsideTx, CodeStatementRejected},
	{exec.ErrLockOutsideTx, CodeStatementRejected},
}

// wireErr maps SURFACE-INDEPENDENT core errors onto the wire; the surface-aware
// (*Server).wireErr method below calls it after resolving the two typed
// backend-acquisition failures whose disclosure DOES depend on the surface.
// The transport withholds any
// non-*Error text (deny-before-disclose), so this is the ONE place autodb
// decides what is deliberately public — and it publishes the MATCHED
// SENTINEL's constant text, never err.Error(): a future wrapper adding
// context ("user 42 from 10.0.0.9: %w") would otherwise export its whole
// contextual string across the disclosure boundary. Structured admission
// refusals retain the established RPC taxonomy while publishing their
// stage-authored actionable Detail. Plain legacy sentinel errors still publish
// only the sentinel's constant text.
// Anything else stays server-side and reaches the peer as a generic internal
// error.
func wireErr(err error) error {
	if err == nil {
		return nil
	}
	if admission.IsOperationalError(err) {
		return err
	}
	if reason, ok := exec.AdmissionReason(err); ok {
		for _, pe := range publicErrs {
			if errors.Is(err, pe.sentinel) {
				return &golibrpc.Error{Code: pe.code, Message: reason.Detail}
			}
		}
		return &golibrpc.Error{Code: CodeStatementRejected, Message: reason.Detail}
	}
	for _, pe := range publicErrs {
		if errors.Is(err, pe.sentinel) {
			return &golibrpc.Error{Code: pe.code, Message: pe.sentinel.Error()}
		}
	}
	return err // transport logs it; peer sees a generic internal error
}

// wireErr (method) is the surface-aware entry point every handler calls. It
// resolves the two TYPED backend-acquisition failures here — because their
// disclosure depends on the SURFACE this daemon serves — then defers every
// surface-independent mapping to the package-level wireErr above.
//
// A *DialFailure / *ConfigFailure carries a cause that names the target host,
// the role, the database and any credential in the DSN (see core/exec). On a
// host-local surface — a unix socket, or a loopback TCP bind, the boundary
// ADR 0056 §4 relies on — that cause is the operator's own to read on their
// own install, so it is disclosed (the same reasoning the CodeKeyslot block
// records). On a surface reachable off-host the cause is WITHHELD and only the
// cause-free sentinel shape crosses, deferring wider exposure to the M9
// gate-guard ADR. Either way this is the RPC surface only: the pgwire front
// door redacts these through a physically separate path (frontdoor/) that this
// method never touches.
func (s *Server) wireErr(err error) error {
	if err == nil {
		return nil
	}
	if de, ok := exec.DialFailureOf(err); ok {
		msg := de.Error() // fixed, cause-free shape
		if s.discloseDetail {
			if c := de.Cause(); c != nil {
				// scrubSecrets, not the raw cause: a driver connect error is
				// measured to carry a plaintext password / PAT / query secret.
				msg = scrubSecrets(c.Error())
			}
		}
		return &golibrpc.Error{Code: CodeDialFailed, Message: msg}
	}
	if ce, ok := exec.ConfigFailureOf(err); ok {
		msg := ce.Error() // fixed, cause-free shape
		if s.discloseDetail {
			if c := ce.Cause(); c != nil {
				msg = scrubSecrets(c.Error())
			}
		}
		return &golibrpc.Error{Code: CodeConfigFailed, Message: msg}
	}
	return wireErr(err)
}

// --- positional argument decoding (msgpack-RPC params are arrays) ---

// exactArgs enforces exact positional arity: trailing extras are an invalid
// call, not silently ignored input.
// argsBetween accepts a verb whose trailing arguments are OPTIONAL.
//
// It exists because adding a required argument to a live verb is a wire break:
// every client pinned to the previous protocol would start failing on a call
// that used to work. An optional trailing argument is additive, and its
// ABSENCE has to mean the safe thing -- for the acknowledgement that permits
// widening an allowlist, absent means "not approved", which is exactly
// today's behaviour.
func argsBetween(p []any, lo, hi int) error {
	if len(p) < lo || len(p) > hi {
		return &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
			Message: fmt.Sprintf("want %d..%d argument(s), got %d", lo, hi, len(p))}
	}
	return nil
}

// strsToAny converts a string slice for the reply.
//
// The codec encodes []any, not []string -- a []string comes back to the caller
// as an INTERNAL ERROR, because the failure is in encoding the response and
// the transport will not say more than that. Every existing handler that
// returns a list does this conversion; two new ones did not, and the wire
// cells are what found it. Without them the preview verb and the
// stale-approval reply would both have failed in front of an operator.
func strsToAny(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}

// optStr reads a trailing optional string argument; absent yields "".
func optStr(p []any, i int, name string) (string, error) {
	if i >= len(p) {
		return "", nil
	}
	return argStr(p, i, name)
}

func exactArgs(p []any, n int) error {
	if len(p) != n {
		return &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
			Message: fmt.Sprintf("want %d argument(s), got %d", n, len(p))}
	}
	return nil
}

func argStr(p []any, i int, name string) (string, error) {
	if i >= len(p) {
		return "", &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
			Message: fmt.Sprintf("missing argument %d (%s)", i, name)}
	}
	s, ok := p[i].(string)
	if !ok {
		return "", &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
			Message: fmt.Sprintf("argument %d (%s): want string, got %T", i, name, p[i])}
	}
	return s, nil
}

func argInt(p []any, i int, name string) (int64, error) {
	if i >= len(p) {
		return 0, &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
			Message: fmt.Sprintf("missing argument %d (%s)", i, name)}
	}
	n, ok := p[i].(int64)
	if !ok {
		return 0, &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
			Message: fmt.Sprintf("argument %d (%s): want int, got %T", i, name, p[i])}
	}
	return n, nil
}

func argBool(p []any, i int, name string) (bool, error) {
	if i >= len(p) {
		return false, &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
			Message: fmt.Sprintf("missing argument %d (%s)", i, name)}
	}
	b, ok := p[i].(bool)
	if !ok {
		return false, &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
			Message: fmt.Sprintf("argument %d (%s): want bool, got %T", i, name, p[i])}
	}
	return b, nil
}

func identMap(id auth.Identity) map[string]any {
	return map[string]any{"id": id.UserID(), "name": id.Name(), "role": id.Role()}
}

// register wires the v1 method surface. Every handler is a
// mechanical projection: decode positional args, call the core with the
// peer IP threaded through, map the result/error. No business logic.
func (s *Server) register() {
	s.handle("sys.hello", s.helloHandler)
	s.registerPressure()
	s.registerM6()

	// --- auth: sessions & bootstrap ---
	s.handle("auth.needs_bootstrap", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 0); err != nil {
			return nil, err
		}
		need, err := s.auth.NeedsBootstrap(ctx)
		return need, s.wireErr(err)
	})
	// auth.global_ip_admitted answers the GLOBAL layer alone, for a caller
	// that has no user to ask about yet.
	//
	// It exists for exactly one moment: the web gateway deciding whether an
	// address may perform the irreversible first-admin bootstrap. At that
	// moment there is no account, so the per-user layer has nothing to
	// consult and auth.ip_admitted — which resolves a token — cannot be
	// asked. Tokenless for the same reason auth.bootstrap is: no token can
	// exist before the first user does.
	//
	// It leaks nothing that the bootstrap form does not already reveal.
	// There are no accounts to enumerate, so there is no username-existence
	// question to protect, which is precisely why the ordering rule for
	// ORDINARY login (credentials first, admission second) does not apply
	// here and its inverse is correct: the address is checked BEFORE the
	// side effect, because the side effect cannot be undone.
	s.handle("auth.global_ip_admitted", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		addr, err := argStr(req.Params, 0, "ip")
		if err != nil {
			return nil, err
		}
		admitted, aerr := s.auth.IPAllowed(ctx, addr)
		if aerr != nil {
			return nil, s.wireErr(aerr)
		}
		return admitted, nil
	})
	s.handle("auth.bootstrap", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		name, err := argStr(req.Params, 0, "name")
		if err != nil {
			return nil, err
		}
		pass, err := argStr(req.Params, 1, "passphrase")
		if err != nil {
			return nil, err
		}
		token, id, err := s.auth.Bootstrap(ctx, name, pass, peerIP(req))
		if err != nil {
			return nil, s.wireErr(err)
		}
		return map[string]any{"token": token, "user": identMap(id)}, nil
	})
	s.handle("auth.login", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		name, err := argStr(req.Params, 0, "name")
		if err != nil {
			return nil, err
		}
		pass, err := argStr(req.Params, 1, "passphrase")
		if err != nil {
			return nil, err
		}
		token, id, err := s.auth.Login(ctx, name, pass, peerIP(req))
		if err != nil {
			return nil, s.wireErr(err)
		}
		return map[string]any{"token": token, "user": identMap(id)}, nil
	})
	// auth.login_at is auth.login with the ADMISSION ADDRESS the caller
	// observed.
	//
	// It exists because the daemon cannot see the browser: its peer is the
	// web gateway over loopback. The gateway used to log in, ask
	// auth.ip_admitted separately, and log the session out again when the
	// answer was no — which made a correct password cost a minted-and-
	// revoked session more than an incorrect one, a difference a caller
	// could time. One verb means the admission decision happens between
	// verification and minting, so a refused caller costs what a wrong
	// password costs and no session is ever created.
	//
	// Tokenless, like auth.login, and it CANNOT weaken anything: the
	// supplied address adds a second layer that auth.login does not have at
	// all, so a caller who forged one would be no better off than by calling
	// auth.login instead. The daemon's own peer allowlist still applies to
	// the connection itself and is not forgeable from here.
	s.handle("auth.login_at", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		name, err := argStr(req.Params, 0, "name")
		if err != nil {
			return nil, err
		}
		pass, err := argStr(req.Params, 1, "passphrase")
		if err != nil {
			return nil, err
		}
		browser, err := argStr(req.Params, 2, "admission_ip")
		if err != nil {
			return nil, err
		}
		token, id, err := s.auth.LoginAt(ctx, name, pass, peerIP(req), browser)
		if err != nil {
			return nil, s.wireErr(err)
		}
		return map[string]any{"token": token, "user": identMap(id)}, nil
	})
	// THE SERVICE KEYSLOT. Three verbs, all admin-gated inside
	// core/auth rather than here — this layer is transport, and an authority
	// check written at the transport is one a second transport forgets.
	//
	// keyslot.enroll is the ONE-TIME step the acceptance story allows: an
	// admin, already logged in, cuts a slot so no later restart needs a human.
	s.handle("keyslot.enroll", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.auth.EnrollServiceKeyslot(ctx, token, peerIP(req)))
	})
	s.handle("keyslot.remove", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.auth.RemoveServiceKeyslot(ctx, token, peerIP(req)))
	})
	// keyslot.status is what makes the locked-daemon banner honest at a DISTANCE. The daemon prints
	// its banner once, at start, to a terminal nobody may be watching; this is
	// how an operator asks later, from the TUI, why every developer is being
	// refused. It reports the LAST ATTEMPT rather than re-reading the keyfile,
	// because the question is "what happened at boot", not "what would happen
	// now".
	s.handle("keyslot.status", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		// ADMIN-ONLY, and the authorization lives in core so this handler
		// cannot be the place the rule is decided.
		//
		// It used to be ValidateToken alone, on the reasoning that "is this
		// install unlocked, and why not" is not an UNAUTHENTICATED caller's
		// question. True, and insufficient: Reason is an err.Error() from the
		// boot unlock attempt and names the keyfile path, so an authenticated
		// editor read operational detail about the master key's protection.
		// R13 -- deny before you disclose.
		st, err := s.auth.ServiceKeyslotStatusFor(ctx, token)
		if err != nil {
			return nil, s.wireErr(err)
		}
		// TWO RECORDS, and they answer different questions. attempted/unlocked/
		// reason are what the BOOT probe found and never change; the verified_*
		// fields are what has been proven SINCE -- an enrolment or a removal.
		// Collapsing them is why a successful enrolment kept reporting the
		// startup failure.
		now := s.auth.ServiceKeyslotNow()
		verifiedAt := ""
		if !now.At.IsZero() {
			verifiedAt = now.At.UTC().Format(time.RFC3339)
		}
		return map[string]any{
			"attempted": st.Attempted,
			"unlocked":  st.Unlocked,
			"reason":    st.Reason,

			"checked":       now.Checked,
			"verified":      now.Verified,
			"verified_at":   verifiedAt,
			"verify_reason": now.Reason,
			"slot_present":  now.SlotPresent,
			// Whether the question could be answered at all. Without it a
			// failed lookup is indistinguishable from a deliberate removal,
			// which is how the UI came to report one as the other.
			"slot_present_known": now.SlotPresenceKnown,
			// The store's CURRENT state, which is not the same question: a
			// failed keyslot followed by a passphrase login leaves attempted
			// false-ish and the store open, and an operator needs both.
			"store_unlocked": s.auth.Unlocked(),
		}, nil
	})
	s.handle("auth.logout", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.auth.Logout(ctx, token, peerIP(req)))
	})
	s.handle("auth.whoami", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		id, err := s.auth.ValidateToken(ctx, token)
		if err != nil {
			return nil, s.wireErr(err)
		}
		return identMap(id), nil
	})

	// --- auth: user management (admin, token-first) ---
	s.handle("auth.user_create", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 4); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		name, err := argStr(req.Params, 1, "name")
		if err != nil {
			return nil, err
		}
		pass, err := argStr(req.Params, 2, "passphrase")
		if err != nil {
			return nil, err
		}
		role, err := argStr(req.Params, 3, "role")
		if err != nil {
			return nil, err
		}
		id, err := s.auth.CreateUser(ctx, token, name, pass, role, peerIP(req))
		if err != nil {
			return nil, s.wireErr(err)
		}
		return id, nil
	})
	// --- auth: the caller's own preferences (no admin gate: your own account) ---
	s.handle("auth.options_get", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		opts, err := s.auth.UserOptions(ctx, token)
		if err != nil {
			return nil, s.wireErr(err)
		}
		// Widened to any: the wire encoder has no view of map[string]string,
		// and the TUI reads it back key by key.
		out := make(map[string]any, len(opts))
		for k, v := range opts {
			out[k] = v
		}
		return out, nil
	})
	s.handle("auth.option_set", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		key, err := argStr(req.Params, 1, "key")
		if err != nil {
			return nil, err
		}
		value, err := argStr(req.Params, 2, "value")
		if err != nil {
			return nil, err
		}
		if err := s.auth.SetUserOption(ctx, token, key, value, peerIP(req)); err != nil {
			return nil, s.wireErr(err)
		}
		return true, nil
	})
	s.handle("auth.user_role", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		role, err := argStr(req.Params, 2, "role")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.auth.SetUserRole(ctx, token, userID, role, peerIP(req)))
	})
	s.handle("auth.user_disable", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		disabled, err := argBool(req.Params, 2, "disabled")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.auth.SetUserDisabled(ctx, token, userID, disabled, peerIP(req)))
	})
	s.handle("auth.user_remove", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.auth.RemoveUser(ctx, token, userID, peerIP(req)))
	})
	s.handle("auth.passphrase_change", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		oldPass, err := argStr(req.Params, 1, "old_passphrase")
		if err != nil {
			return nil, err
		}
		newPass, err := argStr(req.Params, 2, "new_passphrase")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.auth.ChangePassphrase(ctx, token, oldPass, newPass, peerIP(req)))
	})
	// exec.run_script runs a multi-statement buffer sequentially and
	// returns the LAST statement's result. The core splits with the
	// classifier's own lexer and runs each statement through the normal
	// guarded path — the wire adds nothing.
	s.handle("exec.run_script", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 1, "connection_id")
		if err != nil {
			return nil, err
		}
		sqlText, err := argStr(req.Params, 2, "sql")
		if err != nil {
			return nil, err
		}
		// The ATOMIC path (R5 gate). A script containing a transaction
		// boundary runs inside one transaction on an ephemeral session; a
		// script without one behaves exactly as it always has, statement by
		// statement. This is the one call site — the editor surfaces (Lua,
		// TUI, Web) all reach the engine through here, so making them
		// atomic is not three changes.
		out, xerr := s.eng.ExecuteScriptAtomic(ctx, token, connID, sqlText, peerIP(req))
		if xerr != nil {
			return nil, s.wireErr(xerr)
		}
		reply := map[string]any{"statements": int64(out.Statements)}
		if out.Last != nil {
			reply["result"] = resultMap(out.Last)
		}
		return reply, nil
	})

	// history.list is the script-history read side (Objective 5/20). The
	// CORE decides what the caller may see (admins everything, everyone
	// else their own executions) — the wire just projects it.
	s.handle("history.list", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		limit, err := argInt(req.Params, 1, "limit")
		if err != nil {
			return nil, err
		}
		rows, herr := s.eng.ListHistory(ctx, token, int(limit))
		if herr != nil {
			return nil, s.wireErr(herr)
		}
		out := make([]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, map[string]any{
				"id": r.ID, "user_id": r.UserID, "user": r.User,
				"connection_id": r.ConnID, "connection": r.Conn, "ip": r.IP,
				"script": r.Script, "started_at": r.StartedAt.Format(time.RFC3339),
				"duration_ms": r.Duration.Milliseconds(), "row_count": r.RowCount,
				"status": r.Status, "error": r.Error,
				// A SEPARATE KEY, never a fifth status value: status is the
				// durability token and a suspended Execute did commit. A
				// client that does not know the key reads the same status it
				// always did.
				"suspended": r.Suspended,
			})
		}
		return out, nil
	})

	// sys.shutdown drains this server. The shared server
	// outlives its frontends, so restarting it needs an authorized
	// remote path — a rebuilt binary otherwise keeps serving from the
	// old process). Admin-only, audited BEFORE the effect (R6).
	s.handle("sys.shutdown", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		ident, aerr := s.auth.RequireAdmin(ctx, token)
		if aerr != nil {
			return nil, s.wireErr(aerr)
		}
		if err := s.auth.Audit(ctx, ident.UserID(), peerIP(req),
			"server_shutdown", "requested over rpc"); err != nil {
			return nil, s.wireErr(err) // an unaudited privileged effect never happens
		}
		s.RequestShutdown()
		return map[string]any{"stopping": true}, nil
	})

	s.handle("auth.passphrase_reset", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		newPass, err := argStr(req.Params, 2, "new_passphrase")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.auth.ResetPassphrase(ctx, token, userID, newPass, peerIP(req)))
	})

	// --- auth: grants & allowlist (admin, token-first) ---
	s.handle("auth.grant_add", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 4); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 2, "conn_id")
		if err != nil {
			return nil, err
		}
		role, err := argStr(req.Params, 3, "role")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.auth.AddGrant(ctx, token, userID, connID, role, peerIP(req)))
	})
	s.handle("auth.grant_remove", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 2, "conn_id")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.auth.RemoveGrant(ctx, token, userID, connID, peerIP(req)))
	})
	s.handle("auth.allowlist_add", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		cidr, err := argStr(req.Params, 1, "cidr")
		if err != nil {
			return nil, err
		}
		note, err := argStr(req.Params, 2, "note")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.auth.AddAllowedIP(ctx, token, cidr, note, peerIP(req)))
	})
	s.handle("auth.allowlist_remove", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		cidr, err := argStr(req.Params, 1, "cidr")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.auth.RemoveAllowedIP(ctx, token, cidr, peerIP(req)))
	})
	s.handle("auth.allowlist_list", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		cfg, rows, err := s.auth.ListAllowedIPs(ctx, token)
		if err != nil {
			return nil, s.wireErr(err)
		}
		outRows := make([]any, 0, len(rows))
		for _, r := range rows {
			outRows = append(outRows, map[string]any{
				"id": r.ID, "cidr": r.CIDR, "note": r.Note,
				"created_by": r.CreatedBy, "created_at": r.CreatedAt,
			})
		}
		cfgOut := make([]any, 0, len(cfg))
		for _, c := range cfg {
			cfgOut = append(cfgOut, c)
		}
		return map[string]any{"config": cfgOut, "rows": outRows}, nil
	})
	s.handle("auth.user_allowlist_list", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		rows, err := s.auth.UserIPs(ctx, token, userID)
		if err != nil {
			return nil, s.wireErr(err)
		}
		out := make([]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, map[string]any{
				"id": r.ID, "user_id": r.UserID, "cidr": r.CIDR,
				"label": r.Label, "created_at": r.CreatedAt,
			})
		}
		return out, nil
	})
	// Personal Access Tokens. The credential a person pastes
	// into a DSN, managed from the surfaces they already use.
	//
	// The secret appears exactly once, in this reply. It is not recoverable
	// afterwards from anywhere — the store keeps a selector and a SHA-256 —
	// so the reply is the only chance to copy it, and the client is expected
	// to say so.
	// auth.ip_admitted answers the two-layer admission question for an
	// address the DAEMON cannot see for itself.
	//
	// The web gateway reaches the daemon over loopback, so the peer the
	// daemon observes is the gateway, not the browser. Only the gateway
	// knows the browser's address, and only the daemon knows the user's
	// allowlist rows — so one of them has to tell the other. This verb is
	// that, in the direction that keeps the DECISION with the rules: the
	// gateway supplies the address it observed, and the daemon decides.
	//
	// The alternative — the gateway reading auth.user_allowlist_list and
	// evaluating the prefixes itself — would put a second implementation of
	// admission in a second process, and the day they disagree is a day
	// someone is admitted somewhere the rules say they are not.
	s.handle("auth.ip_admitted", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		addr, err := argStr(req.Params, 1, "ip")
		if err != nil {
			return nil, err
		}
		ident, verr := s.auth.ValidateToken(ctx, token)
		if verr != nil {
			return nil, s.wireErr(verr)
		}
		src, aerr := s.auth.IPAllowedForUser(ctx, nil, ident.UserID(), addr)
		if aerr != nil {
			return nil, s.wireErr(aerr)
		}
		// The SOURCE is returned as well as the verdict, so the caller can
		// audit which layer admitted rather than only that something did.
		return map[string]any{
			"admitted": src != auth.NotAdmitted,
			"source":   string(src),
		}, nil
	})

	// auth.token_create takes SIX arguments: conn_id is new and
	// is not optional, because every PAT is bound to exactly one connection and
	// there is no unscoped form. exactArgs is exact, so an old client meets a
	// clean arity error rather than minting something unbound — which is the
	// shape a breaking change should have when the alternative is a credential
	// that silently reaches everything its owner is granted.
	s.handle("auth.token_create", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		// SIX OR SEVEN. The seventh is the operator's acknowledgement: the
		// exact canonical CIDRs they were shown and approved for addition to
		// their OWN allowlist.
		//
		// OPTIONAL, because making it required would break every client on
		// the previous protocol for a call that used to work -- and because
		// its absence has to mean the safe thing anyway: not approved, which
		// leaves the core's subset refusal exactly as it was.
		if err := argsBetween(req.Params, 6, 7); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		name, err := argStr(req.Params, 1, "name")
		if err != nil {
			return nil, err
		}
		days, err := argInt(req.Params, 2, "days")
		if err != nil {
			return nil, err
		}
		rawIPs, err := argStr(req.Params, 3, "allowed_ips")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 4, "conn_id")
		if err != nil {
			return nil, err
		}
		// debug_cleartext. Refused unless the caller is an
		// admin AND this daemon is serving cleartext right now — the engine
		// enforces that, not this handler, so a different caller cannot reach
		// a weaker path.
		debugCleartext, err := argInt(req.Params, 5, "debug_cleartext")
		if err != nil {
			return nil, err
		}
		var ips []string
		if strings.TrimSpace(rawIPs) != "" {
			ips = strings.Split(rawIPs, ",")
		}
		// The integer domain is validated BEFORE the multiply.
		//
		// time.Duration(days)*24*time.Hour overflows for large magnitudes,
		// and overflow wraps: math.MinInt64+1 days came back as a POSITIVE
		// duration of about a day, sailed past the core's range check, and
		// created a token. The core check is correct and was simply handed a
		// number that no longer meant what the caller sent — so the guard
		// belongs here, on the wire value, before any arithmetic touches it.
		const maxTokenDays = 365
		if days < 0 || days > maxTokenDays {
			return nil, s.wireErr(fmt.Errorf("%w: %d days requested; the range is 1..%d, or 0 for "+
				"the default", auth.ErrPATBadExpiry, days, maxTokenDays))
		}
		rawApproved, err := optStr(req.Params, 6, "approved_ips")
		if err != nil {
			return nil, err
		}
		var approved []string
		if strings.TrimSpace(rawApproved) != "" {
			approved = strings.Split(rawApproved, ",")
		}
		out, cerr := s.auth.CreatePAT(ctx, token, name, connID,
			time.Duration(days)*24*time.Hour, ips, debugCleartext != 0, approved, peerIP(req))
		if cerr != nil {
			// A STALE ACKNOWLEDGEMENT IS ITS OWN OUTCOME, carried across the
			// wire as data rather than as prose in an error string: the
			// caller has to tell "needs fresh consent" from a fault so it can
			// RE-PROMPT with the new set instead of reporting a failure.
			var stale *auth.StaleApproval
			if errors.As(cerr, &stale) {
				return map[string]any{
					"stale_approval": true,
					"missing":        strsToAny(stale.Missing),
				}, nil
			}
			return nil, s.wireErr(cerr)
		}
		return map[string]any{
			"name": out.Name,
			// The one and only time this value exists outside the client's
			// hands.
			"secret":     out.Secret,
			"expires_at": out.ExpiresAt.Format(time.RFC3339),
		}, nil
	})

	// What minting with these restrictions would ADD to the caller's own
	// allowlist. Presentation only: the mint recomputes it under the owner's
	// lock and may add nothing that is not in the set then approved.
	s.handle("auth.token_allowlist_preview", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		rawIPs, err := argStr(req.Params, 1, "allowed_ips")
		if err != nil {
			return nil, err
		}
		var want []string
		if strings.TrimSpace(rawIPs) != "" {
			want = strings.Split(rawIPs, ",")
		}
		missing, merr := s.auth.PATAllowlistAdditions(ctx, token, want)
		if merr != nil {
			return nil, s.wireErr(merr)
		}
		return map[string]any{"missing": strsToAny(missing)}, nil
	})
	s.handle("auth.token_list", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		rows, lerr := s.auth.ListPATs(ctx, token, userID)
		if lerr != nil {
			return nil, s.wireErr(lerr)
		}
		out := make([]any, 0, len(rows))
		for _, r := range rows {
			// Never the digest and never the selector. The digest is
			// useless to a client and the selector is half the credential:
			// publishing it would turn a token list into a head start.
			out = append(out, map[string]any{
				"name":        r.Name,
				"created_at":  time.Unix(r.CreatedAt, 0).Format(time.RFC3339),
				"expires_at":  time.Unix(r.ExpiresAt, 0).Format(time.RFC3339),
				"last_used":   lastUsedString(r.LastUsedAt),
				"revoked":     r.IsRevoked(),
				"allowed_ips": r.AllowedIPs,
			})
		}
		return out, nil
	})

	s.handle("auth.token_revoke", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		name, err := argStr(req.Params, 2, "name")
		if err != nil {
			return nil, err
		}
		if rerr := s.auth.RevokePAT(ctx, token, userID, name); rerr != nil {
			return nil, s.wireErr(rerr)
		}
		return map[string]any{"revoked": true}, nil
	})

	s.handle("auth.user_allowlist_add", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 4); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		cidr, err := argStr(req.Params, 2, "cidr")
		if err != nil {
			return nil, err
		}
		label, err := argStr(req.Params, 3, "label")
		if err != nil {
			return nil, err
		}
		// An empty cidr means "the address this request came from" — the
		// self-service enrollment gesture; only the server knows that
		// address truthfully, so the substitution happens here.
		if cidr == "" {
			cidr = peerIP(req)
		}
		return nil, s.wireErr(s.auth.AddUserIP(ctx, token, userID, cidr, label, peerIP(req)))
	})
	s.handle("auth.user_allowlist_remove", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		rowID, err := argInt(req.Params, 2, "row_id")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.auth.RemoveUserIP(ctx, token, userID, rowID, peerIP(req)))
	})

	// --- conn: connection management ---
	s.handle("conn.create", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 4); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		name, err := argStr(req.Params, 1, "name")
		if err != nil {
			return nil, err
		}
		engineArg, err := argStr(req.Params, 2, "engine")
		if err != nil {
			return nil, err
		}
		// PARSE AT THE BOUNDARY. This is where an untrusted string becomes an
		// engine.Name, and the only place in the daemon that converts one. A
		// caller that sends "Postgres" gets a readable refusal here rather than
		// a row that no later comparison matches.
		eng, err := engine.Parse(engineArg)
		if err != nil {
			return nil, s.wireErr(err)
		}
		dsn, err := argStr(req.Params, 3, "dsn")
		if err != nil {
			return nil, err
		}
		id, err := s.eng.CreateConnection(ctx, token, name, eng, dsn, peerIP(req))
		if err != nil {
			return nil, s.wireErr(err)
		}
		return id, nil
	})
	s.handle("conn.list", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		conns, err := s.eng.ListConnections(ctx, token)
		if err != nil {
			return nil, s.wireErr(err)
		}
		// THE EFFECTIVE CAP, NOT THE REQUEST.
		//
		// A connection row's PoolMaxConns is a REQUEST -- poolLimitsFor keeps
		// the SMALLER of it and the engine's own ceiling, and a row asking for
		// more than the install allows simply does not get it. Publishing the
		// row value made the card advertise 100 pooled connections on an
		// install that would grant 8, under a heading promising ceilings the
		// caller will actually meet.
		//
		// Read once, outside the loop: every row is bounded by the same engine
		// ceiling, and taking it per row would let a concurrent reload split
		// one list across two policies.
		engineCeiling := s.eng.Settings().PoolMaxConns
		effectivePool := func(request int64) int64 {
			// Zero means the row asks for nothing of its own, so the engine's
			// ceiling is the whole answer.
			if request <= 0 || int(request) > engineCeiling {
				return int64(engineCeiling)
			}
			return int64(request)
		}
		out := make([]any, 0, len(conns))
		for _, c := range conns {
			out = append(out, map[string]any{
				"id": c.ID, "name": c.Name, "engine": c.Engine.String(),
				"created_by": c.CreatedBy, "created_at": c.CreatedAt,
				"updated_at": c.UpdatedAt,
				// Exposure and capability are separate facts. Returning both
				// prevents clients from reconstructing one from the other.
				"profile": c.Profile, "frontdoor_exposed": c.FrontDoorExposed != 0,
				"target_db": c.TargetDB,
				// WHAT THIS CONNECTION WILL ACTUALLY BE HELD TO: the row's
				// request and the engine's ceiling, whichever binds. Additive,
				// so no protocol bump -- the verb surface is unchanged.
				"pool_max_conns": effectivePool(c.PoolMaxConns),
			})
		}
		return out, nil
	})
	s.handle("conn.delete", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 1, "conn_id")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.eng.DeleteConnection(ctx, token, connID, peerIP(req)))
	})
	// frontdoor.endpoint reports what a client must dial. It takes the bearer
	// token and re-resolves authority like every other privileged verb
	// (security-core-hardening R1) — the values look harmless, but "where does
	// this install expose a database surface" is not a question an
	// unauthenticated caller gets to ask.
	//
	// It reports the LIVE listener, never config intent: a card printing the
	// configured bind while the listener failed to start would send a
	// developer to debug their client.
	// THE CA CERTIFICATE ITSELF, not its path.
	//
	// It is the one file every client needs -- `sslmode=verify-full` plus this
	// -- and the path was useless to the person who needs it: a developer
	// running the TUI over a tunnel cannot read a file on the daemon's host,
	// and on the host itself /etc/autodb/tls is 0710 so only root and the
	// service account can even traverse it.
	//
	// AUTHENTICATED, NOT ADMIN-ONLY. A CA certificate is public by
	// construction -- it is what you hand out -- and every developer who has
	// to configure a client needs it. Gating it on admin would mean root
	// couriering a public file to each of them.
	//
	// THE PATH COMES FROM CONFIG, NEVER FROM THE CALLER. This reads only the
	// configured tls_root_ca_file; a caller-supplied path would make this an
	// arbitrary-file-read verb wearing a certificate's name.
	s.handle("frontdoor.ca_pem", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		if _, err := s.auth.ValidateToken(ctx, token); err != nil {
			return nil, s.wireErr(err)
		}
		var info FrontDoorInfo
		if s.frontDoor != nil {
			info = s.frontDoor()
		}
		if strings.TrimSpace(info.RootCAFile) == "" {
			// No private CA configured: the client should trust the system
			// roots, and saying so is more useful than an empty document.
			return map[string]any{"path": "", "pem": "", "system_roots": true}, nil
		}
		pem, rerr := os.ReadFile(info.RootCAFile)
		if rerr != nil {
			return nil, s.wireErr(fmt.Errorf("reading the CA certificate at %s: %w",
				info.RootCAFile, rerr))
		}
		return map[string]any{
			"path": info.RootCAFile,
			"pem":  string(pem),
			// Reported so a reader is not left wondering whether an empty
			// document means "system roots" or "unreadable".
			"system_roots": false,
		}, nil
	})
	s.handle("frontdoor.endpoint", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		if _, err := s.auth.ValidateToken(ctx, token); err != nil {
			return nil, s.wireErr(err)
		}
		var info FrontDoorInfo
		if s.frontDoor != nil {
			info = s.frontDoor()
		}
		return map[string]any{
			"enabled":      info.Enabled,
			"listening":    info.Listening,
			"addr":         info.Addr,
			"host_names":   toAnyList(info.HostNames),
			"root_ca_file": info.RootCAFile,
			"cleartext":    info.Cleartext,

			// ADDITIVE, so no protocol bump: an older frontend ignores these
			// and a newer one reads absent as absent. The verb surface is
			// unchanged, which is what the handshake number tracks.
			"max_sessions_per_user": int64(info.MaxSessionsPerUser),
			"max_sessions_global":   int64(info.MaxSessionsGlobal),
			"max_target_conns":      int64(info.MaxTargetConns),
		}, nil
	})

	// conn.set_profile is an audited capability change, independent of network
	// exposure.
	s.handle("conn.set_profile", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 1, "conn_id")
		if err != nil {
			return nil, err
		}
		profile, err := argStr(req.Params, 2, "profile")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.eng.SetConnectionProfile(ctx, token, connID, profile, peerIP(req)))
	})
	s.handle("conn.rename", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 1, "conn_id")
		if err != nil {
			return nil, err
		}
		name, err := argStr(req.Params, 2, "name")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.eng.RenameConnection(ctx, token, connID, name, peerIP(req)))
	})
	s.handle("conn.set_exposure", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 1, "conn_id")
		if err != nil {
			return nil, err
		}
		exposed, err := argBool(req.Params, 2, "exposed")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.eng.SetConnectionExposure(ctx, token, connID, exposed, peerIP(req)))
	})
	// The operator surface for the reloadable bounds. Without these the
	// engine's reload existed and nothing could reach it, which is the state
	// the accepted policy called out by name: a setter reachable only from a
	// program embedding the engine is not an operator surface.
	s.handle("policy.show", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		return s.eng.ShowPolicy(ctx, token)
	})
	s.handle("policy.reload", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 6); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		// Milliseconds, not duration strings. The wire carries a number that
		// means one thing, rather than a format whose parser has to agree on
		// both sides of a version boundary.
		names := []string{"session_idle_ms", "idle_in_tx_ms", "max_tx_ms", "max_tx_ceiling_ms", "max_target_conns"}
		vals := make([]int64, len(names))
		for i, name := range names {
			v, ierr := argInt(req.Params, i+1, name)
			if ierr != nil {
				return nil, ierr
			}
			vals[i] = v
		}
		// RANGE-CHECKED BEFORE MULTIPLICATION. int64 milliseconds times
		// time.Millisecond overflows above roughly 2.9e8 years, and Go wraps
		// silently -- so a large positive input would arrive as a negative
		// duration, or as a small one, and be validated as though the operator
		// had asked for it.
		const maxMS = int64(math.MaxInt64) / int64(time.Millisecond)
		for i, name := range names[:4] {
			if vals[i] < 0 || vals[i] > maxMS {
				return nil, fmt.Errorf("%s is %d; it must be between 0 and %d milliseconds, "+
					"beyond which the conversion wraps into a different duration",
					name, vals[i], maxMS)
			}
		}
		ms := func(v int64) time.Duration { return time.Duration(v) * time.Millisecond }
		set, rerr := s.eng.ReloadPolicy(ctx, token, exec.PolicySpec{
			SessionIdleTimeout:   ms(vals[0]),
			IdleInTxTimeout:      ms(vals[1]),
			MaxTxDuration:        ms(vals[2]),
			MaxTxDurationCeiling: ms(vals[3]),
			MaxTargetConns:       int(vals[4]),
		}, peerIP(req))
		if rerr != nil {
			return nil, s.wireErr(rerr)
		}
		return exec.PolicyView(set), nil
	})
	s.handle("conn.test", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 1, "conn_id")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.eng.TestConnection(ctx, token, connID, peerIP(req)))
	})

	// --- exec ---
	// The ExecSession surface. A session is the client's
	// handle on a PINNED connection: it is what makes a transaction spanning
	// several round trips possible at all, since the stateless path may put
	// each statement on a different physical connection.
	//
	// The session id is opaque and engine-issued. It is deliberately NOT the
	// auth token and NOT the TCP connection: a client may hold several, and
	// one dying must not take the others with it.
	s.handle("exec.session_open", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 1, "conn_id")
		if err != nil {
			return nil, err
		}
		sid, oerr := s.eng.OpenSession(ctx, token, connID, peerIP(req))
		if oerr != nil {
			return nil, s.wireErr(oerr)
		}
		return map[string]any{"session_id": string(sid)}, nil
	})

	// Closing is not optional housekeeping: an open session holds a physical
	// connection, and closing rolls back any transaction still open on it.
	// A client that crashes without calling this is reaped by the engine's
	// idle timeout — this verb is the polite path, not the only one.
	s.handle("exec.session_close", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		sid, err := argStr(req.Params, 1, "session_id")
		if err != nil {
			return nil, err
		}
		if cerr := s.eng.CloseSession(ctx, token, exec.SessionID(sid), peerIP(req)); cerr != nil {
			return nil, s.wireErr(cerr)
		}
		return map[string]any{"closed": true}, nil
	})

	// Session-scoped run. The connection is the SESSION's — the caller does
	// not pass one, and cannot redirect a session at another connection
	// mid-transaction.
	s.handle("exec.session_run", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		sid, err := argStr(req.Params, 1, "session_id")
		if err != nil {
			return nil, err
		}
		sqlText, err := argStr(req.Params, 2, "sql")
		if err != nil {
			return nil, err
		}
		res, rerr := s.eng.SessionExecute(ctx, token, exec.SessionID(sid), sqlText, peerIP(req))
		if rerr != nil {
			return nil, s.wireErr(rerr)
		}
		return resultMap(res), nil
	})

	// tx.status is the poll verb for asynchronous outcome delivery.
	//
	// It reads the ENGINE's outcome API, never the table, and never the
	// script history — history disappears entirely when [history].enabled is
	// false, and a boundary-only `BEGIN; COMMIT;` never had a row there at
	// all, so a projection over it could not answer for the two cases that
	// most need answering.
	//
	// One tx id gives that transaction's resolved state; no id gives the
	// caller's unresolved ones, oldest first. Both are scoped in core: a
	// transaction that is not the caller's answers exactly as one that never
	// existed, so the id space cannot be used to discover what exists.
	s.handle("tx.status", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		n := len(req.Params)
		if n != 2 && n != 3 {
			return nil, &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
				Message: "tx.status takes (token, tx_id) or (token, \"\", limit)"}
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		txID, err := argStr(req.Params, 1, "tx_id")
		if err != nil {
			return nil, err
		}
		if txID != "" {
			// The single-transaction form takes exactly two arguments. A
			// third alongside a non-empty id is not a defined shape, and
			// accepting it silently would let a caller write
			// tx.status(token, id, limit) — a reading that looks obvious and
			// means nothing here — and receive an answer about the id while
			// their limit was quietly discarded. A verb that ignores an
			// argument teaches the wrong contract to whoever copies the call.
			if n != 2 {
				return nil, &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
					Message: "tx.status with a tx_id takes exactly (token, tx_id); " +
						"a limit applies only to the pending form (token, \"\", limit)"}
			}
			st, terr := s.eng.TxOutcome(ctx, token, txID)
			if terr != nil {
				return nil, s.wireErr(terr)
			}
			return txStatusMap(st), nil
		}
		limit := 0
		if n == 3 {
			l, lerr := argInt(req.Params, 2, "limit")
			if lerr != nil {
				return nil, lerr
			}
			limit = int(l)
		}
		list, perr := s.eng.PendingOutcomes(ctx, token, limit)
		if perr != nil {
			return nil, s.wireErr(perr)
		}
		out := make([]any, 0, len(list))
		for _, st := range list {
			out = append(out, txStatusMap(st))
		}
		return map[string]any{"pending": out}, nil
	})

	s.handle("exec.run", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 1, "conn_id")
		if err != nil {
			return nil, err
		}
		sqlText, err := argStr(req.Params, 2, "sql")
		if err != nil {
			return nil, err
		}
		res, err := s.eng.Execute(ctx, token, connID, sqlText, peerIP(req))
		if err != nil {
			return nil, s.wireErr(err)
		}
		return resultMap(res), nil
	})
}

// txStatusMap projects one transaction outcome onto the wire.
//
// `terminal` is computed HERE and not carried on the Go struct: on the engine
// side it is a method, because a precomputed field can be constructed
// inconsistently with the state it is derived from. A Lua or Web client
// cannot call a Go method, so this is the layer where precomputing earns its
// keep — one place, derived from the state it ships alongside.
//
// Times go out as RFC3339Nano strings like every other time on this wire, and
// `stuck_ms` is the gap between them: how long this transaction has been in
// its current state is the number that decides whether to act, and making
// each client subtract two timestamps is how three clients get three answers.
func txStatusMap(st exec.TxStatus) map[string]any {
	return map[string]any{
		"tx_id":    st.TxID,
		"state":    string(st.State),
		"reason":   st.Reason,
		"conn_id":  st.ConnID,
		"user_id":  st.UserID,
		"terminal": st.Terminal(),
		"opened":   st.Opened.Format(time.RFC3339Nano),
		"since":    st.Since.Format(time.RFC3339Nano),
		"stuck_ms": time.Since(st.Since).Milliseconds(),
	}
}

// resultMap projects exec.Result onto the wire in the exec.run shape.
// Row cells are normalized into the msgpack vocabulary — drivers hand back
// types the wire doesn't carry (pgx timestamps as time.Time), and one
// exotic cell must not fail the whole result page.
func resultMap(res *exec.Result) map[string]any {
	cols := make([]any, 0, len(res.Columns))
	for _, c := range res.Columns {
		cols = append(cols, c)
	}
	rows := make([]any, 0, len(res.Rows))
	for _, r := range res.Rows {
		row := make([]any, len(r))
		for i, v := range r {
			row[i] = wireVal(v)
		}
		rows = append(rows, row)
	}
	return map[string]any{
		"verb":        res.Verb,
		"class":       string(res.Class),
		"columns":     cols,
		"rows":        rows,
		"more":        res.More,
		"affected":    res.Affected,
		"duration_ms": res.Duration.Milliseconds(),
	}
}

// wireVal normalizes one result cell. Types the codec carries pass through;
// time.Time becomes RFC3339Nano; a 16-byte fixed ARRAY (a postgres uuid) takes
// its canonical dashed form, because this is the last point it is
// distinguishable from a bytea; anything else stringifies — the FEs are
// display surfaces, and a lossy-but-visible cell beats a failed page.
func wireVal(v any) any {
	switch x := v.(type) {
	case nil, bool, int64, float64, string, []byte,
		int, int8, int16, int32, uint, uint8, uint16, uint32, uint64, float32:
		return x
	case time.Time:
		return x.Format(time.RFC3339Nano)
	}
	// Fixed-size byte ARRAYS never match the []byte case above: a
	// postgres uuid scans into [16]uint8, and the %v fallback would ship
	// it as a decimal byte list.
	//
	// A uuid (fixed ARRAY) and a bytea (SLICE) are distinct types HERE, and
	// the same anonymous bytes once either is on the wire. So the canonical
	// text form is decided here, at the last point the type still exists —
	// not at the frontend, which cannot recover the difference and renders
	// both through its generic binary-to-hex fallback. The earlier note that
	// "how they read is the frontend's decision" described an ability the Lua
	// frontend never had: it receives bytes with no type beside them.
	//
	// Length is part of the match rather than an afterthought. The premise is
	// that only uuid scans into a fixed-size byte array, which is a property
	// of pgx and not of this code; formatting exactly 16 bytes means a future
	// codec returning some other fixed array stays visible bytes instead of
	// being silently mislabelled as a uuid.
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Array &&
		rv.Type().Elem().Kind() == reflect.Uint8 {
		b := make([]byte, rv.Len())
		reflect.Copy(reflect.ValueOf(b), rv)
		if len(b) == 16 {
			return fmt.Sprintf("%x-%x-%x-%x-%x",
				b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
		}
		return b
	}
	// Driver types that know their own text form (pgtype values, decimals,
	// net.IP, …) render through it rather than through %v's struct dump.
	switch x := v.(type) {
	case encoding.TextMarshaler:
		if t, err := x.MarshalText(); err == nil {
			return string(t)
		}
	case fmt.Stringer:
		return x.String()
	}
	return fmt.Sprintf("%v", v)
}

// registerM6 wires the schema.* and workspace.* surface (protocol 2)
// under the same per-verb discipline as the v1 methods: exact
// arity and typed decoding, peer-IP threading into every mutating core
// call, sentinel-constant-only disclosure, wire-vocabulary normalization.
func (s *Server) registerM6() {
	// --- schema introspection (authz ≥ reader happens in the core, R13) ---
	s.handle("schema.schemas", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 1, "conn_id")
		if err != nil {
			return nil, err
		}
		names, err := s.eng.ListSchemas(ctx, token, connID)
		if err != nil {
			return nil, s.wireErr(err)
		}
		out := make([]any, 0, len(names))
		for _, n := range names {
			out = append(out, n)
		}
		return out, nil
	})
	s.handle("schema.tables", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 1, "conn_id")
		if err != nil {
			return nil, err
		}
		schema, err := argStr(req.Params, 2, "schema")
		if err != nil {
			return nil, err
		}
		tables, err := s.eng.ListTables(ctx, token, connID, schema)
		if err != nil {
			return nil, s.wireErr(err)
		}
		out := make([]any, 0, len(tables))
		for _, t := range tables {
			out = append(out, map[string]any{
				"schema": t.Schema, "name": t.Name,
				"kind": string(t.Kind), "quoted": t.Quoted,
				"partitioned": t.Partitioned, "is_partition": t.IsPartition,
				"parent": t.Parent,
			})
		}
		return out, nil
	})
	s.handle("schema.columns", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 4); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 1, "conn_id")
		if err != nil {
			return nil, err
		}
		schema, err := argStr(req.Params, 2, "schema")
		if err != nil {
			return nil, err
		}
		table, err := argStr(req.Params, 3, "table")
		if err != nil {
			return nil, err
		}
		cols, err := s.eng.ListColumns(ctx, token, connID, schema, table)
		if err != nil {
			return nil, s.wireErr(err)
		}
		out := make([]any, 0, len(cols))
		for _, c := range cols {
			out = append(out, map[string]any{
				"name": c.Name, "type": c.DataType, "nullable": c.Nullable,
				"default": c.Default, "has_default": c.HasDefault,
				"position": int64(c.Position), "pk": c.PrimaryKey,
			})
		}
		return out, nil
	})
	s.handle("schema.routines", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 1, "conn_id")
		if err != nil {
			return nil, err
		}
		schema, err := argStr(req.Params, 2, "schema")
		if err != nil {
			return nil, err
		}
		supported, routines, err := s.eng.ListRoutines(ctx, token, connID, schema)
		if err != nil {
			return nil, s.wireErr(err)
		}
		list := make([]any, 0, len(routines))
		for _, r := range routines {
			list = append(list, map[string]any{
				"schema": r.Schema, "name": r.Name,
				"kind": string(r.Kind), "signature": r.Signature,
			})
		}
		// Capability absence is DATA (supported=false), never an error.
		return map[string]any{"supported": supported, "routines": list}, nil
	})

	s.handle("auth.user_list", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		users, err := s.auth.ListUsers(ctx, token)
		if err != nil {
			return nil, s.wireErr(err)
		}
		out := make([]any, 0, len(users))
		for _, u := range users {
			out = append(out, map[string]any{
				"id": u.ID, "name": u.Name, "role": u.Role, "disabled": u.Disabled,
			})
		}
		return out, nil
	})

	// --- workspaces ---
	s.handle("workspace.create", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		name, err := argStr(req.Params, 1, "name")
		if err != nil {
			return nil, err
		}
		id, err := s.eng.CreateWorkspace(ctx, token, name, peerIP(req))
		if err != nil {
			return nil, s.wireErr(err)
		}
		return id, nil
	})
	s.handle("workspace.list", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		views, err := s.eng.ListWorkspaces(ctx, token)
		if err != nil {
			return nil, s.wireErr(err)
		}
		out := make([]any, 0, len(views))
		for _, w := range views {
			conns := make([]any, 0, len(w.Connections))
			for _, c := range w.Connections {
				conns = append(conns, map[string]any{
					"id": c.ID, "name": c.Name, "engine": c.Engine.String(),
				})
			}
			out = append(out, map[string]any{
				"id": w.ID, "name": w.Name, "created_at": w.CreatedAt,
				"connections": conns,
			})
		}
		return out, nil
	})
	s.handle("workspace.rename", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		wsID, err := argInt(req.Params, 1, "workspace_id")
		if err != nil {
			return nil, err
		}
		name, err := argStr(req.Params, 2, "name")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.eng.RenameWorkspace(ctx, token, wsID, name, peerIP(req)))
	})
	s.handle("workspace.delete", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		wsID, err := argInt(req.Params, 1, "workspace_id")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.eng.DeleteWorkspace(ctx, token, wsID, peerIP(req)))
	})
	s.handle("workspace.attach", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		wsID, err := argInt(req.Params, 1, "workspace_id")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 2, "conn_id")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.eng.AttachConnection(ctx, token, wsID, connID, peerIP(req)))
	})
	s.handle("workspace.detach", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		wsID, err := argInt(req.Params, 1, "workspace_id")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 2, "conn_id")
		if err != nil {
			return nil, err
		}
		return nil, s.wireErr(s.eng.DetachConnection(ctx, token, wsID, connID, peerIP(req)))
	})
}

// lastUsedString renders a PAT's last-use time, or "never".
//
// Zero is not a date. Rendering it as 1970 would put a plausible-looking
// timestamp on a token nobody has used, which is exactly the row an operator
// is scanning for when they are deciding what to revoke.
func lastUsedString(unix int64) string {
	if unix == 0 {
		return "never"
	}
	return time.Unix(unix, 0).Format(time.RFC3339)
}

// toAnyList widens a string slice for the wire codec, which encodes []any.
func toAnyList(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}

// registerPressure exposes the front door's pressure view.
//
// ADMIN-ONLY FOR NOW, AND DELIBERATELY SO RATHER THAN BY DEFAULT. The view
// discloses operational detail about other people: which users hold sessions,
// which source addresses are being held out, and what is being refused. Nothing
// in the design settles who may read that, so it follows the rule keyslot.status
// was corrected to — deny before you disclose.
//
// The capability is what matters now and the audience is a separate decision
// that is Johno's to make. Widening this is one line, and narrowing it after
// somebody has built on it is not, so it starts narrow.
func (s *Server) registerPressure() {
	s.handle("sys.pressure", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		// The authorization lives in core, so this handler cannot be the place
		// the rule is decided.
		if _, err := s.auth.RequireAdminToken(ctx, token); err != nil {
			return nil, s.wireErr(err)
		}
		if s.pressure == nil {
			return nil, s.wireErr(errNoPressure)
		}
		snap, err := s.pressure()
		if err != nil {
			return nil, s.wireErr(err)
		}
		return pressureWire(snap), nil
	})
}

// errNoPressure is returned rather than an empty view, because an empty view and
// a quiet front door read the same on a screen.
var errNoPressure = errors.New("pressure is not being observed on this instance")

// pressureWire projects the view onto plain values.
//
// AN EXPLICIT WIRE SHAPE, NOT THE INTERNAL STRUCT. Returning the snapshot
// directly made every field of an internal type part of a protocol: renaming one
// would silently change what a frontend receives, and a type the encoder cannot
// handle -- a Duration, say -- becomes an "internal error" a caller cannot act
// on. That is how this was found: a cell required an admin to be ANSWERED, not
// merely not-denied, and the answer was a marshalling failure.
//
// Durations are rendered as whole seconds, because the receiver is a surface
// somebody reads and a wait in nanoseconds is a true figure nobody can use.
func pressureWire(s pressure.Snapshot) map[string]any {
	row := func(r pressure.Row) map[string]any {
		return map[string]any{
			"label": r.Label, "subject": r.Subject,
			"value": r.Value, "cap": r.Cap, "raised": r.Raised,
		}
	}
	rows := func(in []pressure.Row) []any {
		out := make([]any, 0, len(in))
		for _, r := range in {
			out = append(out, row(r))
		}
		return out
	}
	denials := make([]any, 0, len(s.Denials))
	for _, d := range s.Denials {
		denials = append(denials, map[string]any{
			"reason": d.Reason, "class": d.Class.String(), "count": d.Count,
		})
	}
	throttled := make([]any, 0, len(s.Throttled))
	for _, th := range s.Throttled {
		throttled = append(throttled, map[string]any{
			"host": th.Host, "remaining_seconds": int64(th.Remaining / time.Second),
		})
	}
	return map[string]any{
		"sessions": row(s.Sessions),
		"conns":    row(s.Conns),
		"pre_auth": row(s.PreAuth),

		"per_user":          rows(s.PerUser),
		"per_user_omitted":  s.PerUserOmitted,
		"leases":            rows(s.Leases),
		"leases_omitted":    s.LeasesOmitted,
		"denials":           denials,
		"denials_omitted":   s.DenialsOmitted,
		"throttled":         throttled,
		"throttled_omitted": s.ThrottledOmitted,
	}
}
