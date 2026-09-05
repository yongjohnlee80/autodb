package exec

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgconn"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"
	"sort"
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
	// ParameterStatuses is every ParameterStatus the pinned target connection
	// reported at its own connect (matrix §3.3): the loop forwards them verbatim
	// at session open, overriding only is_superuser and application_name. Empty
	// for non-postgres targets.
	ParameterStatuses map[string]string
	// ApplicationName is the client's accepted startup application_name, as
	// recorded on the session (matrix claim 3.1:application_name#session-audit).
	ApplicationName string
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
	// DenyPATUnscoped: the presented token carries conn_id = 0 — a pre-v13
	// token, or a v13 tombstone somebody un-revoked by hand (ADR-0086 §2).
	// Refused INDEPENDENTLY of `revoked`, so re-flipping that column cannot
	// bring an unscoped credential back.
	DenyPATUnscoped = "frontdoor/pat-unscoped"
	// DenyDatabaseMismatch: the startup `database` agrees with neither the
	// bound connection's name, nor its target_db, nor conn:<id> (ADR-0086 §4).
	//
	// NEVER silently substituted: a client that asked for `postgres` and was
	// quietly given `test` is worse than any refusal.
	DenyDatabaseMismatch = "frontdoor/database-mismatch"
	// DenyPATNotCleartextDebug: a cleartext listener met an ordinary token
	// (ADR-0086 §10). Not charged — the credential is valid and the peer may be
	// fully admitted; what refuses it is our mode.
	DenyPATNotCleartextDebug = "frontdoor/pat-not-cleartext-debug"
	// DenyPATCleartextDebugInTLS: a TLS listener met a debug_cleartext token.
	// The exact mirror of the above, and uncharged for the same reason — the
	// token is perfectly valid on the listener it was minted for.
	DenyPATCleartextDebugInTLS = "frontdoor/pat-cleartext-debug-in-tls"
	DenyNoGrant                = "frontdoor/no-grant"
	DenyProfileRefuses         = "frontdoor/profile-not-front-door"
	DenyLeaseCap               = "frontdoor/lease-cap-exceeded"
	DenySessionCap             = "frontdoor/session-cap-exceeded"
	DenyResidentBudget         = "frontdoor/resident-budget-exceeded"
	// DenyLeaseEncoding: the pinned target's server_encoding or client_encoding
	// is not UTF8, or could not be established (matrix row 3.1: the lease is
	// pinned UTF8; autodb does not transcode; the check FAILS CLOSED). On the
	// wire it is the uniform 28000 like every lease failure in the reservation
	// phase (§7 ruling 4); this reason is the audit identity only.
	DenyLeaseEncoding = "frontdoor/lease-encoding"
	// DenyStartupGUC: a startup parameter named a setting this session may not
	// change (the Amendment 8 denylist), or the target refused to apply it.
	// Uniform 28000 on the wire (§7 ruling 4); the audit row names the key.
	DenyStartupGUC = "frontdoor/startup-parameter-refused"
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
// WireOpen is everything a front-door session open needs from the startup
// exchange. ApplicationName is the CLIENT's label, already length-capped by the
// front door; it is recorded on the session and in its audit lines.
type WireOpen struct {
	PAT, StartupUser, Database, IP, ApplicationName string
	// StartupGUCs are the StartupMessage parameters outside row 3.1's named
	// set — the settings a client asks for at connect (lib/pq's datestyle,
	// JDBC's TimeZone and extra_float_digits). Each is admitted EXACTLY as
	// `SET name TO value` from this session would be (ADR-0075 Amendment 8,
	// one admission implementation) and, when admitted, applied to the pinned
	// backend before the result returns — PostgreSQL's own semantics, not an
	// emulation. One refusal withdraws the session (DenyStartupGUC).
	StartupGUCs map[string]string
	// Cleartext reports that the LISTENER is serving without TLS (ADR-0086
	// §10). It comes from the listener rather than from config because it is a
	// property of the connection being opened, and because the engine must not
	// have to re-derive a fact the caller already knows.
	Cleartext bool
}

// OpenWireSession is the original entry point, kept so the front door's
// interface keeps compiling; it opens with no application_name.
func (e *Engine) OpenWireSession(ctx context.Context, presented, startupUser, database, ip string) (WireSessionResult, error) {
	return e.OpenWireSessionWith(ctx, WireOpen{PAT: presented, StartupUser: startupUser, Database: database, IP: ip})
}

// OpenWireSessionWith authenticates a front-door client, admits the session,
// and — for postgres targets — PINS the session's backend connection at open,
// so the target's reported ParameterStatus set can be handed to the loop before
// its first frame and the row-3.1 lease rule (UTF8) is enforced before any
// statement runs. Pinning at open rather than at the first statement is what
// makes §3.3 satisfiable at all: the set is a property of the connection.
func (e *Engine) OpenWireSessionWith(ctx context.Context, req WireOpen) (WireSessionResult, error) {
	presented, startupUser, database, ip := req.PAT, req.StartupUser, req.Database, req.IP
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

	// 3a. THE CREDENTIAL CLASS MUST MATCH THE LISTENER (ADR-0086 §10).
	//
	// Checked BEFORE any IP work, and that placement is load-bearing:
	// auth.PATAllowsIP returns TRUE for an empty list by contract, because
	// empty means "inherit the admission set" and that is correct under TLS.
	// Teaching it about cleartext would silently change TLS mode too, so this
	// is a separate check with its own reason rather than a condition bolted
	// into a function whose contract other callers depend on.
	//
	// Both directions are refusals, and the token is VALID in each — what
	// refuses it is the listener's mode. That is why neither charges the
	// per-address throttle (§5): a developer whose daemon restarted into the
	// other mode is the common case, not an attacker.
	debugToken := pat.DebugCleartext != 0
	switch {
	case req.Cleartext && !debugToken:
		// Turning cleartext ON refuses every ordinary token in the install.
		// That is the point: a normal credential must never cross a wire that
		// does not protect it.
		return out, deny(DenyPATNotCleartextDebug)
	case !req.Cleartext && debugToken:
		// A debug token's allowed_ips was never checked against its owner's
		// perimeter, so honouring it here would import that relaxation into
		// production. This refusal is what confines the widening to the mode
		// it was minted for.
		return out, deny(DenyPATCleartextDebugInTLS)
	}

	// 3b. IP admission.
	//
	// Under TLS: (global ∨ the user's rows), then the token's own narrowing if
	// it sets one (ADR-0075 Amendment 1).
	//
	// In CLEARTEXT: the token's own list is the ENTIRE gate and the inherited
	// set is NOT consulted (ADR-0086 §10, ruled by Johno). The list is
	// guaranteed non-empty by the mint gate; it is re-checked here anyway,
	// because a store edited by hand must not yield a token admitted from
	// anywhere.
	src := auth.AdmittedByTokenList
	if !req.Cleartext {
		var aerr error
		src, aerr = e.auth.IPAllowedForUser(ctx, nil, pat.UserID, ip)
		if aerr != nil {
			return out, aerr
		}
		if src == auth.NotAdmitted {
			return out, deny(DenyIPNotAdmitted)
		}
	} else if strings.TrimSpace(pat.AllowedIPs) == "" {
		return out, deny(DenyPATIPNarrowed)
	}
	if !auth.PATAllowsIP(pat.AllowedIPs, ip) {
		return out, deny(DenyPATIPNarrowed)
	}

	// 4. THE TARGET COMES FROM THE CREDENTIAL, NOT FROM THE CLIENT (ADR-0086 §1).
	//
	// This is the change that dissolves the ambiguity rather than managing it.
	// Two connections targeting a database called `test` — a local one and a
	// PRODUCTION one — are told apart by WHICH TOKEN was presented, not by a
	// string the client chose, so there is no spelling a client can send that
	// reaches a connection its token does not name.
	//
	// The tombstone is refused here rather than in VerifyPAT: the denial
	// vocabulary lives in this package, and VerifyPAT deliberately collapses
	// every credential failure into one error. It is also AFTER the
	// constant-time compare, so it cannot become a timing oracle for which
	// selectors exist.
	if pat.ConnID == 0 {
		return out, deny(DenyPATUnscoped)
	}
	connRow, err := e.store.Connections.OnCtx(ctx).With(meta.ConnID, pat.ConnID).Get()
	if err != nil {
		if errors.Is(err, dao.ErrNoRows) {
			// A live token naming a row that is gone. There is no database-level
			// foreign key (ADR-0086 §1: it cannot be added to a populated
			// table), so this is the guard that makes a dangling reference
			// LOUD rather than silent, and it is why this reason survived the
			// binding change instead of being collapsed into the mismatch.
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

	// 4c. The `database` field is now a CONSISTENCY CHECK, not a lookup key
	// (ADR-0086 §4, ruling R2).
	//
	// Checked AFTER the grant so an ungranted caller never produces a
	// mismatch row: a reader of the audit trail would take that row as
	// evidence the connection exists.
	if !wireDatabaseAgrees(database, connRow) {
		return out, deny(DenyDatabaseMismatch)
	}

	// 5. ONE atomic reservation: per-user slot, global slot, target lease,
	// and this session's fixed overhead.
	id, err := newSessionID()
	if err != nil {
		return out, err
	}
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &session{
		// The authority is the TOKEN, named by its row id. A zero sentinel
		// here is what made the janitor read every wire session as revoked.
		id: id, userID: pat.UserID, authority: auth.PATAuthority(pat.ID), connID: connRow.ID,
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

	s.mu.Lock()
	s.appName, s.wire = req.ApplicationName, true
	s.mu.Unlock()

	var statuses map[string]string
	if connRow.Engine == "postgres" {
		// Pin now. A failure here is the target's, not the client's: the
		// admitted session is withdrawn and the error is returned unframed.
		pc, perr := e.pinWireSession(ctx, s, connRow)
		if perr != nil {
			e.sessions.remove(s)
			cancel()
			return out, perr
		}
		// Row 3.1 fails CLOSED: no reporter, no statuses, or either encoding
		// key missing is a refusal, not an acceptance — the lease's encoding
		// must be ESTABLISHED as UTF8, not merely not-known-to-be-otherwise.
		if r, ok := e.reporterFor(pc).(golibpg.ParameterStatusReporter); ok {
			statuses = r.ReportedParameterStatuses()
		}
		if enc, ok := leaseEncodingRefusal(statuses); ok {
			// Row 3.1: the lease is pinned UTF8; autodb does not transcode.
			e.auditBounded(ctx, pat.UserID, ip, "wire_lease_encoding_refused",
				fmt.Sprintf("conn %d: session %s: target reports %s", connRow.ID, s.id, enc))
			pc.Discard()
			s.mu.Lock()
			s.pc = nil
			s.mu.Unlock()
			e.sessions.remove(s)
			cancel()
			return out, deny(DenyLeaseEncoding)
		}
		// Startup GUCs (Amendment 8): judged by the SAME denylist a SET from
		// this session meets, then applied to the pinned backend so the
		// reported ParameterStatus set the client receives already reflects
		// them. Any refusal — ours or the target's — withdraws the session.
		if len(req.StartupGUCs) > 0 {
			if gerr := e.applyStartupGUCs(ctx, s, pc, req.StartupGUCs, pat.UserID, ip, connRow.ID); gerr != nil {
				pc.Discard()
				s.mu.Lock()
				s.pc = nil
				s.mu.Unlock()
				e.sessions.remove(s)
				cancel()
				return out, deny(DenyStartupGUC)
			}
			if r, ok := e.reporterFor(pc).(golibpg.ParameterStatusReporter); ok {
				statuses = r.ReportedParameterStatuses() // live: reflects the SETs
			}
		}
	} else if len(req.StartupGUCs) > 0 {
		// A non-PostgreSQL target has no session to apply a GUC to. Refuse
		// rather than pretend: an accepted setting that silently did nothing
		// is the behaviour a client cannot detect.
		e.auditBounded(ctx, pat.UserID, ip, "wire_startup_guc_refused",
			fmt.Sprintf("conn %d: session %s: target engine %q has no session settings", connRow.ID, s.id, connRow.Engine))
		e.sessions.remove(s)
		cancel()
		return out, deny(DenyStartupGUC)
	}
	e.auth.NotePATUse(ctx, pat)
	return WireSessionResult{
		SessionID: id, UserID: pat.UserID, ConnID: connRow.ID,
		AdmissionSource: src, PATName: pat.Name, UserName: owner.Name,
		ParameterStatuses: statuses, ApplicationName: req.ApplicationName,
	}, nil
}

// leaseEncodingRefusal reports why the lease cannot be established as UTF8:
// no reported set, a missing server_encoding or client_encoding, or a value
// that is not UTF8. SQL_ASCII is refused too: it means the server validates
// nothing, which is not "UTF8-compatible" for a relay that does not transcode.
// Absence is a refusal — the rule fails closed.
func leaseEncodingRefusal(statuses map[string]string) (string, bool) {
	if statuses == nil {
		return "no reported statuses", true
	}
	for _, k := range []string{"server_encoding", "client_encoding"} {
		v, ok := statuses[k]
		if !ok {
			return k + " missing", true
		}
		switch strings.ToUpper(strings.ReplaceAll(v, "-", "")) {
		case "UTF8":
		default:
			return k + "=" + v, true
		}
	}
	return "", false
}

// reporterFor returns the value whose ParameterStatusReporter capability is
// consulted at open: the pinned connection itself, or, in tests, whatever
// hookWrapPinned substitutes (a wrapper WITHOUT the capability, or one that
// reports an incomplete set) so the fail-closed arms can be observed.
func (e *Engine) reporterFor(pc golibpg.PinnedConn) any {
	if e.hookWrapPinned != nil {
		return e.hookWrapPinned(pc)
	}
	return pc
}

// wireDatabaseAgrees reports whether the client's startup `database` names the
// connection its token is bound to (ADR-0086 §4).
//
// It REPLACED a lookup. The field used to select which connection to open;
// now the token has already decided that, and this only asks whether the
// client agrees. The distinction matters because it changes what a
// disagreement means: not "no such database" but "you asked for something
// other than what this credential reaches", which is refused rather than
// silently substituted.
//
// Three accepted spellings:
//
//   - the connection's NAME — what works today;
//   - its TARGET DATABASE NAME, when one is known — what every introspecting
//     client sends after discovering the target's real databases, and the
//     whole reason this ADR exists. Skipped when empty, which is the ordinary
//     case for an engine with no derivable name (r7);
//   - `conn:<id>` — the explicit disambiguator, which survives a rename.
//
// TrimSpace and ParseInt are KEPT deliberately. Both are today's behaviour —
// a trailing space in a client's config field is a typo rather than an
// intent, and `conn:01` names the same row as `conn:1` — so tightening them
// here would be a silent change to who can connect, smuggled in under a
// change about something else.
//
// The name comparisons are EXACT BYTES, and that is not the same rule the
// startup `user` gets a few lines above (EqualFold). The asymmetry is each
// namespace's own: two connections whose names differ only in case COEXIST
// (`connections.name` is UNIQUE but case-sensitive on both engines), and
// PostgreSQL database names are case-sensitive too, so folding either would
// accept a client that asked for a DIFFERENT REAL THING. `user` can fold
// because identity comes from the token and no other principal the client
// could have meant would then be served.
func wireDatabaseAgrees(database string, connRow *meta.Connection) bool {
	database = strings.TrimSpace(database)
	if rest, ok := strings.CutPrefix(database, "conn:"); ok {
		id, perr := strconv.ParseInt(rest, 10, 64)
		return perr == nil && id == connRow.ID
	}
	if database == connRow.Name {
		return true
	}
	return connRow.TargetDB != "" && database == connRow.TargetDB
}

// CloseWireSession releases a front-door session and everything it reserved.
func (e *Engine) CloseWireSession(ctx context.Context, id SessionID, userID int64, ip, reason string) {
	s, err := e.sessions.lookup(id, userID)
	if err != nil {
		return
	}
	e.closeSession(ctx, s, ip, reason)
}

// applyStartupGUCs admits and applies the client's startup settings on the
// pinned backend, in sorted order so refusals are deterministic. The admission
// is admitWireSet with Local=false — the same function a SET statement meets —
// and the application is a real `SET name TO 'value'` on the session's own
// backend (PostgreSQL applies startup parameters as session settings; so do
// we, on the session that is now ours). The target's refusal (an unknown
// setting, a bad value — the cases PostgreSQL would FATAL at startup) is a
// refusal here too, audited with the target's own message.
func (e *Engine) applyStartupGUCs(ctx context.Context, s *session, pc golibpg.PinnedConn, gucs map[string]string, userID int64, ip string, connID int64) error {
	pol, perr := e.resolveUnitPolicy(ctx, s.authority, s.userID, s.connID)
	if perr != nil {
		return perr
	}
	sq, ferr := rawFace(pc)
	if ferr != nil {
		return ferr
	}
	names := make([]string, 0, len(gucs))
	for k := range gucs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, raw := range names {
		name := strings.ToLower(raw)
		if err := admitWireSet(setStatement{Name: name}, pol.ReadOnly, false); err != nil {
			e.auditBounded(ctx, userID, ip, "wire_startup_guc_refused",
				fmt.Sprintf("conn %d: session %s: %s: %v", connID, s.id, name, err))
			return err
		}
		var targetErr *pgconn.PgError
		_, derr := sq.SimpleQuery(ctx, "SET "+quoteIdent(name)+" TO "+quoteLiteral(gucs[raw]), func(m golibpg.ExtendedMessage) error {
			if m.Kind == "ErrorResponse" && targetErr == nil {
				targetErr = m.Err
			}
			return nil
		})
		switch {
		case derr != nil:
			e.auditBounded(ctx, userID, ip, "wire_startup_guc_refused",
				fmt.Sprintf("conn %d: session %s: %s: wire: %v", connID, s.id, name, derr))
			return derr
		case targetErr != nil:
			e.auditBounded(ctx, userID, ip, "wire_startup_guc_refused",
				fmt.Sprintf("conn %d: session %s: %s: target: %s", connID, s.id, name, targetErr.Message))
			return targetErr
		}
	}
	e.auditBounded(ctx, userID, ip, "wire_startup_gucs_applied",
		fmt.Sprintf("conn %d: session %s: %s", connID, s.id, strings.Join(names, ",")))
	return nil
}

// quoteIdent renders a setting name as a double-quoted identifier.
func quoteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// quoteLiteral renders a value as a single-quoted string literal. PostgreSQL
// accepts the quoted form for every GUC type (numeric and boolean included).
func quoteLiteral(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }
