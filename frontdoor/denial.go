package frontdoor

import (
	"io"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/outcome"
)

// The uniform external denial (matrix §1.2 and row 2.7).
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
	// uses for "running, but not accepting connections yet" (the
	// locked-daemon rule). It is the ONE state that answers differently from
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
	// WHAT IT BUYS is the locked-daemon contract's honesty: it keeps the daemon RUNNING when
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
	// reasonStoreLocked is the ONLY reason that changes what the wire says.
	// It is not the caller's fault and is never charged to
	// their address.
	reasonStoreLocked denialReason = "frontdoor/store-locked"

	reasonPlaintextStartup denialReason = "frontdoor/tls-required"
	// Emitted as a TLS failure rather than a denial (matrix row 2.1a), which
	// is why it is a reason string and never reaches denial().
	reasonDirectTLS         denialReason = "direct-tls-unsupported"
	reasonUnsupportedMajor  denialReason = "frontdoor/protocol-major-unsupported"
	reasonStartupMalformed  denialReason = "frontdoor/startup-malformed"
	reasonStartupParamRefus denialReason = "frontdoor/startup-parameter-refused"
	// The two ways a startup packet fails the amended parameter rule at PARSE, before the
	// engine judges anything. Both reach the wire as the SAME uniform denial a
	// refused setting does — a distinguishable refusal would map the accepted
	// set for anyone willing to ask repeatedly — and differ only here, in the
	// audit, so the operator record keeps what the wire deliberately discards
	// (ruled 2026-09-03).
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
	reasonSourceThrottled denialReason = "frontdoor/source-ip-throttled"
	reasonConnectionCap   denialReason = "frontdoor/connection-cap"
	// reasonSourceConnCap: one address already holds its share of concurrent
	// connections. Distinct from reasonSourceThrottled, which is about FAILED
	// credentials: this peer may have presented nothing wrong at all and is
	// simply using more than its allowance, so an operator reading
	// "source-ip-throttled" would go looking for a credential attack that
	// never happened.
	reasonSourceConnCap        denialReason = "frontdoor/source-connection-cap"
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
// CapacitySQLState and CapacityMessage are what a fully authorized caller is
// told when the system is full.
//
// 53300 too_many_connections is a code every client already understands, so
// DataGrip and psql render it sensibly instead of showing a dropped socket.
// The message is FIXED and carries no cause: which cap was reached is an
// operator's business, kept in the audit trail.
const (
	CapacitySQLState = "53300"
	CapacityMessage  = "the database is at its connection limit; try again shortly"
)

// denial renders a refusal for a caller who has NOT proved who they are.
func denial(reason denialReason) *pgproto3.ErrorResponse {
	// NO WITNESS, NO CHARGE: the zero occurrence can only render uniformly.
	return denialForOccurrence(outcome.Occurrence{Reason: outcome.ReasonID(reason)})
}

// denialFor renders a refusal, disclosing capacity only to an authorized
// caller.
//
// THE SECOND ARGUMENT IS A WITNESS, NOT A FLAG DERIVED FROM THE REASON. It
// comes from the engine, set only where a refusal was raised with a verified
// credential in hand. That is what makes the exemption survive a reordering:
// move a capacity check above the credential check and no witness exists, so
// this returns the uniform denial rather than leaking to a stranger.
//
// There is deliberately NO reason-to-code table. A table is how a uniform
// surface becomes an enumerable one, one well-meaning row at a time.
// denialForOccurrence is the ONE projection that may disclose capacity, and it
// takes the typed occurrence rather than a Boolean.
//
// THE RULE IS A CONJUNCTION: authorized AND registered as capacity.
//
// It used to be the witness alone, and that was wrong in a way the tests
// agreed with. The witness proves WHO the caller is; it does not prove WHAT
// happened to them. A refusal raised after authorization for any other cause --
// a missing grant, a refused profile, our own stored state -- would have been
// rendered to the peer as "the database is at its connection limit", which is
// both false and a disclosure nobody authorised: it reports the system's load
// to a caller whose actual problem was something else entirely.
//
// Neither fact implies the other, so both are required. Removing either term
// reopens one of the two holes.
func denialForOccurrence(occ outcome.Occurrence) *pgproto3.ErrorResponse {
	if occ.Disclosable && occ.Charge == outcome.Capacity {
		return capacityFrame()
	}
	return denialUniform(denialReason(occ.Reason))
}

func capacityFrame() *pgproto3.ErrorResponse {
	return &pgproto3.ErrorResponse{
		Severity:            "FATAL",
		SeverityUnlocalized: "FATAL",
		Code:                CapacitySQLState,
		Message:             CapacityMessage,
		// The stable rule id, exactly as every other denial: constant, never
		// the cause.
		Detail: "frontdoor/capacity",
	}
}

func denialUniform(reason denialReason) *pgproto3.ErrorResponse {
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

// InternalSQLState and InternalMessage are what an AUTHENTICATED caller is
// told when our own lifecycle fails underneath them.
//
// 58000 internal_error is PostgreSQL's own code for exactly this, so every
// client library already renders it as a server fault rather than as anything
// the caller did. The message is fixed and carries no cause: which phase broke
// is an operator's business, and the peer learns only that the server failed.
//
// It is sent ONLY to a caller who has already authenticated and only on a
// quiescent stream. Before authentication the uniform denial is the whole of
// what anyone is owed; mid-exchange, a frame would corrupt the stream.
const (
	InternalSQLState = "58000"
	InternalMessage  = "the server failed while handling this connection"
)

// sendFatalInternal writes the stable internal-error frame.
func sendFatalInternal(w io.Writer) error {
	be := pgproto3.NewBackend(emptyReader{}, w)
	be.Send(&pgproto3.ErrorResponse{
		Severity:            "FATAL",
		SeverityUnlocalized: "FATAL",
		Code:                InternalSQLState,
		Message:             InternalMessage,
		Detail:              "frontdoor/internal",
	})
	return be.Flush()
}

// THE ONE CLIENT SHAPE FOR A FAILED BACKEND ACQUISITION.
//
// Every way a backend connection can fail to open — a name that will not
// resolve, a socket that will not connect, a TLS handshake that will not
// complete, a startup the target refuses, the credential autodb itself
// presents, the settings re-applied to a fresh backend — reaches the client as
// exactly these three values, and the stage and the raw cause go to the audit
// trail alone.
//
// What goes wrong otherwise is that the text upstream produces describes OUR
// topology: it names the target host and port, the database, and the role
// autodb connects as. A client is entitled to the answer to its own statement
// and to nothing about the estate behind the front door, and a surface that
// varied by cause would let anyone with a TCP route enumerate that estate one
// broken target at a time.
//
// THE OBVIOUS ALTERNATIVE IS A CLASS 08 CODE, AND IT IS THE ONE TO AVOID.
// 08006 connection_failure says what happened in the plainest words the
// standard has. What makes it the wrong choice is that class 08 is the class
// clients and connection pools attach their OWN recovery rules to, and several
// act on the two-character prefix alone: a pool that evicts on class 08 throws
// the session away even where the driver would have kept it, which converts
// "this request failed" into "your session died" without a single frame being
// wrong. The promise this shape makes is that the session survives, so the
// code must not be one whose fate is decided by each client's class policy.
//
// MEASURED, so the sentence above does not overclaim: neither pgx v5 nor
// pgjdbc 42.7.4 closes the session on an ERROR-severity 08006 today. That is a
// fact about two current versions rather than about the protocol, and it is
// exactly the kind of fact a pool in front of either one overrides. The choice
// does not rest on it.
//
// 58030 io_error is PostgreSQL's own code for "an I/O operation failed" and is
// what a real server raises for a file it could not read — a server-side fault
// that leaves the session exactly where it was, in a class nothing recovers
// from by convention. The severity is ERROR rather than FATAL for the same
// reason: FATAL means the backend is closing the connection, and this one is
// not.
//
// THE CODE IS A VERIFIED CHOICE, NOT AN ASSERTED ONE. Both required clients
// keep the session usable across it in the simple AND the extended protocol:
// real pgx in dial_failed_pg_test.go, and a real pgjdbc program in
// dial_failed_jdbc_pg_test.go, which reads back the driver's own SQLSTATE,
// severity, closed and isValid and then runs real work on the same connection.
// docs/front-door/dial-failed-client-verification.md is how to run either by
// hand.
const (
	DialFailedSQLState = "58030"

	// DialFailedMessage is the WHOLE of what the wire learns. No cause, no
	// stage, no host, no DETAIL that varies: two dial failures for two
	// different reasons are byte-identical to the client.
	DialFailedMessage = "the database connection for this request could not be established"

	// DialFailedHint states the property the shape promises, because a client
	// that cannot tell a failed request from a failed session will throw the
	// session away and reconnect — which is the load a dial failure can least
	// afford.
	DialFailedHint = "the session is still usable; send the statement again"

	// DialFailedRule is the stable rule id that travels in DETAIL, exactly as
	// every other front-door refusal carries one: constant, and never the
	// cause.
	//
	// DEFINED FROM THE REGISTRY IDENTITY RATHER THAN BESIDE IT, because an
	// identity spelled twice is an identity that can be spelled two ways. It
	// was: the registry declared "dial-failed" while every raise site wrote
	// this value, so the declaration named an outcome nothing could produce
	// and the wire and the audit trail named one nothing had declared. Neither
	// half could detect the other, because the two strings never met. One
	// constant, one spelling, and the rule id a client quotes is the identity
	// an operator greps for.
	DialFailedRule = OutcomeDialFailed
)

// A CONNECTION AUTODB CANNOT SERVE AS CONFIGURED GETS ITS OWN FIXED SHAPE.
//
// It is not a dial failure and must not borrow that code: an operator reading
// a client's complaint should land on a different row than a target outage,
// because the repair is a connection row or a dependency rather than a
// network. It is not the internal-fault shape either, which is FATAL and ends
// the connection -- this session is intact and every other connection it can
// reach still works.
//
// F0000 config_file_error is PostgreSQL's own class for "this server's
// configuration is wrong", raised at ERROR severity, which is exactly the
// claim being made and exactly the severity that leaves the session where it
// was. Class F0 is not in any client's reconnect-or-discard convention.
//
// THAT LAST SENTENCE IS AN ARGUMENT, AND THE MEASUREMENT IS IN
// connection_unusable_pg_test.go: real pgx against a real target reads the
// code and the severity out of its own error type, is not closed by it, and
// then runs real work on the same connection. The JDBC half has NOT been run
// for this shape -- the dial-failure shape has a written manual procedure and
// this one does not yet -- so F0000 rests on one client of the two, and saying
// so here is the point of saying it at all.
const (
	ConnectionUnusableSQLState = "F0000"

	// ConnectionUnusableMessage is the WHOLE of what the wire learns. No
	// stage, no engine, no DSN text, no connection name: two connections
	// misconfigured two different ways are byte-identical to the client.
	ConnectionUnusableMessage = "this connection is not configured to serve requests"

	// ConnectionUnusableHint points at the only party who can act, without
	// telling the client anything about what is wrong.
	ConnectionUnusableHint = "the session is still usable; ask an operator to check this connection's configuration"

	// ConnectionUnusableRule is the stable rule id that travels in DETAIL,
	// defined FROM the registry identity rather than beside it so the two
	// cannot drift apart unnoticed.
	ConnectionUnusableRule = OutcomeConnectionUnusable
)
