package frontdoor

import (
	"github.com/jackc/pgx/v5/pgproto3"
)

// The uniform external denial (ADR-0075 §4, matrix §1.2 and row 2.7).
//
// EVERY external failure after the perimeter — an unknown user, a bad token,
// an IP the user is not allowed from, a database that does not exist, a
// missing grant, a full lease cap — returns this one shape. The internal
// audit row is precise; the wire says only that authorization failed.
//
// The point is that the surface answers no questions. A denial that varied
// by cause would let anyone with a TCP route enumerate which usernames exist
// and which databases are reachable, one connection at a time, without ever
// holding a credential. That the enumeration is slow does not make it a
// smaller hole — it makes it one nobody notices.
const (
	// DenialSQLState is 28000 invalid_authorization_specification: the code
	// PostgreSQL itself uses when authorization fails, so ordinary clients
	// report it in the words their users already know.
	DenialSQLState = "28000"

	// DenialMessage is deliberately the whole of what the wire learns.
	DenialMessage = "authentication failed"

	// LockedSQLState is 57P03 cannot_connect_now — the code PostgreSQL itself
	// uses for "running, but not accepting connections yet" (ADR-0087
	// Amendment 1 A1.3). It is the ONE state that answers differently from
	// DenialSQLState, and the exception is argued rather than assumed:
	//
	// R13 forbids a denial that varies BY CALLER or BY RESOURCE — "an
	// ungranted caller's error for an existing resource must be
	// indistinguishable from a nonexistent one". A locked store is NEITHER.
	// It is a global server state, identical for every caller, independent of
	// whether the token is valid, the user exists, or the connection exists,
	// so it cannot be used to learn anything about a resource and is not an
	// oracle in R13's sense. What it discloses is that this server is up and
	// not serving — the least sensitive state a service has, and one every
	// client library already renders as "not ready" rather than "your
	// credentials are wrong".
	//
	// WHAT IT BUYS is ADR-0087 §6's honesty. §6 keeps the daemon RUNNING when
	// a keyfile fails, justified by the state being loud and visible; a log
	// line is not loud to the person who hits it, and that person is a
	// developer holding a perfectly good token on the morning after a reboot.
	// Telling them their credentials are invalid sends them to regenerate a
	// token that was never the problem.
	LockedSQLState = "57P03"

	// LockedMessage is FIXED. No cause, no HINT, no varying DETAIL: the
	// caller learns the STATE and never the reason, which stays in the
	// operational log and the admin surface.
	LockedMessage = "the server is not accepting connections: the autodb store is locked"
)

// denialReason is the INTERNAL cause. It reaches the audit trail and never
// the wire, so operators can tell these apart and callers cannot.
type denialReason string

const (
	// reasonStoreLocked is the ONLY reason that changes what the wire says
	// (ADR-0087 A1.3). It is not the caller's fault and is never charged to
	// their address.
	reasonStoreLocked denialReason = "frontdoor/store-locked"

	reasonPlaintextStartup denialReason = "frontdoor/tls-required"
	// Emitted as a TLS failure rather than a denial (matrix row 2.1a), which
	// is why it is a reason string and never reaches denial().
	reasonDirectTLS         denialReason = "direct-tls-unsupported"
	reasonUnsupportedMajor  denialReason = "frontdoor/protocol-major-unsupported"
	reasonStartupMalformed  denialReason = "frontdoor/startup-malformed"
	reasonStartupParamRefus denialReason = "frontdoor/startup-parameter-refused"
	// The two ways a startup packet fails Amendment 8 at PARSE, before the
	// engine judges anything. Both reach the wire as the SAME uniform denial a
	// refused setting does — a distinguishable refusal would map the accepted
	// set for anyone willing to ask repeatedly — and differ only here, in the
	// audit, so the operator record keeps what the wire deliberately discards
	// (jarvis, ruling 2, 2026-09-03).
	reasonStartupGUCCount         denialReason = "frontdoor/startup-guc-count"
	reasonStartupOptionsMalformed denialReason = "frontdoor/startup-options-malformed"
	// A key named twice — as two raw wire pairs, twice inside `options`, or once
	// in each. Its own reason rather than a reuse of the options one: a packet
	// carrying no options at all can hit it, and an audit row saying
	// "options-malformed" would send an operator looking for a string that was
	// never sent.
	reasonStartupDuplicateKey denialReason = "frontdoor/startup-duplicate-key"
	reasonPreAuthOversize     denialReason = "frontdoor/pre-auth-message-too-large"
	reasonNoCredentialStore   denialReason = "frontdoor/auth-not-yet-available"

	// Accept-time refusals (matrix §1.4, §9). None of these reaches the
	// wire either: a peer refused for capacity learns only that the
	// connection closed, which is all a peer refused for anything learns.
	reasonSourceThrottled      denialReason = "frontdoor/source-ip-throttled"
	reasonConnectionCap        denialReason = "frontdoor/connection-cap"
	reasonPreAuthConnCap       denialReason = "frontdoor/pre-auth-connection-cap"
	reasonControlLaneExhausted denialReason = "frontdoor/control-lane-exhausted"

	// Authentication-phase refusals (rows 2.6-2.9).
	//
	// reasonAuthStoreError is deliberately NOT an authentication denial in
	// the audit trail even though the wire shape is identical: the peer may
	// have presented a perfectly good credential and been refused because
	// our own store was unreachable. Filing that under fd.auth_denied would
	// inflate the number an operator watches for credential attacks with
	// events that are our fault, and it is the same distinction row 2.1a
	// draws between a TLS failure and a denial.
	reasonPreAuthProtocolViolation denialReason = "frontdoor/pre-auth-protocol-violation"
	reasonAuthStoreError           denialReason = "frontdoor/auth-store-error"
)

// denial builds the wire error. It takes the reason so a caller cannot
// forget to record one, and drops it on the floor for the wire — the reason
// is for the audit row the caller writes, not for the client.
//
// DETAIL carries the front-door rule id rather than the cause, per matrix
// §1.2: a synthesized error must never impersonate the target, and the rule
// id is stable, which is what makes an audit trail greppable.
func denial(reason denialReason) *pgproto3.ErrorResponse {
	// THE ONE EXCEPTION, and it is a single named state rather than a table.
	//
	// The shape matters as much as the decision: a map from reason to code is
	// how a uniform surface becomes an enumerable one, one well-meaning entry
	// at a time. This is an `if` against one constant so that adding a second
	// exception is a visible edit to this function that a reviewer meets,
	// rather than a row somebody appends elsewhere.
	if reason == reasonStoreLocked {
		return &pgproto3.ErrorResponse{
			Severity:            "FATAL",
			SeverityUnlocalized: "FATAL",
			Code:                LockedSQLState,
			Message:             LockedMessage,
			// The stable rule id, exactly as every other denial carries
			// "frontdoor/denied": constant, and not the cause.
			Detail: string(reasonStoreLocked),
		}
	}
	_ = reason
	return &pgproto3.ErrorResponse{
		Severity:            "FATAL",
		SeverityUnlocalized: "FATAL",
		Code:                DenialSQLState,
		Message:             DenialMessage,
		Detail:              "frontdoor/denied",
	}
}

// String makes a reason usable in an audit detail without a conversion at
// every call site.
func (r denialReason) String() string { return string(r) }
