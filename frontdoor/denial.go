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
)

// denialReason is the INTERNAL cause. It reaches the audit trail and never
// the wire, so operators can tell these apart and callers cannot.
type denialReason string

const (
	reasonPlaintextStartup denialReason = "frontdoor/tls-required"
	// Emitted as a TLS failure rather than a denial (matrix row 2.1a), which
	// is why it is a reason string and never reaches denial().
	reasonDirectTLS         denialReason = "direct-tls-unsupported"
	reasonUnsupportedMajor  denialReason = "frontdoor/protocol-major-unsupported"
	reasonStartupMalformed  denialReason = "frontdoor/startup-malformed"
	reasonStartupParamRefus denialReason = "frontdoor/startup-parameter-refused"
	reasonPreAuthOversize   denialReason = "frontdoor/pre-auth-message-too-large"
	reasonNoCredentialStore denialReason = "frontdoor/auth-not-yet-available"

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
