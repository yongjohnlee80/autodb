package frontdoor

import (
	"fmt"

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
	reasonPlaintextStartup  denialReason = "frontdoor/tls-required"
	reasonDirectTLS         denialReason = "frontdoor/direct-tls-unsupported"
	reasonUnsupportedMajor  denialReason = "frontdoor/protocol-major-unsupported"
	reasonStartupMalformed  denialReason = "frontdoor/startup-malformed"
	reasonStartupParamRefus denialReason = "frontdoor/startup-parameter-refused"
	reasonPreAuthOversize   denialReason = "frontdoor/pre-auth-message-too-large"
	reasonNoCredentialStore denialReason = "frontdoor/auth-not-yet-available"
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

var _ = fmt.Sprintf
