package exec

import (
	"errors"
	"fmt"
)

// A CONNECTION AUTODB IS NOT CONFIGURED TO SERVE IS NOT A TARGET OUTAGE, AND
// SAYING SO COSTS NOTHING AND CLAIMS NOTHING.
//
// The two are told apart by who has to act. A dial failure is a fact about the
// target: the host is down, the certificate expired, the upstream credential
// was refused. Nobody reading autodb's own configuration can fix it, and a
// second attempt is reasonable because the target may come back. A
// configuration failure is a fact about THIS install: the connection row names
// an engine autodb does not implement, its stored DSN does not parse, or the
// driver it resolves to cannot destroy a pinned backend on demand. No number
// of retries changes any of those, and the repair is an edit to a connection
// row or a dependency, not an investigation of a network.
//
// FILING ONE AS THE OTHER CORRUPTS BOTH TRAILS. A dial failure records
// Attempts, which is how an operator tells a target that failed once from one
// being retried in a loop; a configuration failure that borrows that shape
// reports an acquisition count for acquisitions that never happened, because
// no permit was taken and no socket was opened. The operator reading it goes
// looking at a network that was never touched. Reviewed and ruled: only a
// physical pin and the sanitation that follows it may be DialFailed.
//
// THE CLIENT LEARNS THE SAME NOTHING AS FOR A DIAL FAILURE, for the same
// reason and by the same mechanism. The raw detail here names the connection,
// the engine, and whatever the DSN parser objected to, which is autodb's
// topology rather than the caller's statement.

// ConfigStage names WHICH part of the connection's configuration is wrong. It
// is an audit fact: the wire shape is identical for every value.
type ConfigStage string

const (
	// ConfigStageEngine is a connection row naming an engine this build does
	// not implement.
	ConfigStageEngine ConfigStage = "engine"
	// ConfigStageDSN is a stored DSN the engine's own parser rejects, or one
	// carrying an option that would desynchronize the statement classifier
	// from the target's grammar.
	ConfigStageDSN ConfigStage = "dsn"
	// ConfigStagePool is the driver's pool object refusing to be constructed.
	//
	// CONSTRUCTION IS NOT A PIN. Building a pgxpool parses a config and
	// allocates; it opens no socket and takes no permit. An error here is a
	// configuration error that happens to surface inside a driver call, and
	// counting it as an acquisition attempt would be counting a dial that
	// never happened.
	ConfigStagePool ConfigStage = "pool"
	// ConfigStageCapability is a resolved driver that does not offer an
	// operation this product's guarantees depend on — today, destroying a
	// pinned backend on demand.
	ConfigStageCapability ConfigStage = "capability"
)

// ErrConnectionUnusable is the sentinel every configuration failure carries,
// so a renderer can recognise one without knowing any stage.
var ErrConnectionUnusable = errors.New("exec: this connection cannot serve requests as it is configured")

// ConfigFailure is one connection that cannot serve requests as configured.
//
// IT DELIBERATELY DOES NOT UNWRAP TO ITS CAUSE, for the reason DialFailure
// does not: the front door's renderers ask errors.As whether an error is a
// *pgconn.PgError or a structured refusal, and those questions walk the whole
// chain. A DSN parser's complaint can carry the host and the role; an Unwrap
// chain would let it match an arm that forwards it verbatim, and the comment
// saying "do not forward this" would have been true while the code forwarded
// it anyway. The cause is reachable only through Cause, which nothing on the
// wire path calls.
type ConfigFailure struct {
	// Stage is which part of the configuration is wrong.
	Stage ConfigStage
	cause error
}

// NewConfigFailure builds one for a stage the caller already knows.
//
// THE STAGE IS ALWAYS PASSED, NEVER CLASSIFIED FROM THE CAUSE. Unlike a dial
// failure, whose cause arrives from a driver and has to be interpreted, every
// configuration failure is raised by a call site in this package that knows
// exactly which check it just failed. Inferring it would be guessing at
// something already known.
func NewConfigFailure(stage ConfigStage, cause error) *ConfigFailure {
	return &ConfigFailure{Stage: stage, cause: cause}
}

// Error names the stage and nothing else. The cause is deliberately absent:
// this text reaches operator logs through paths that do not distinguish audit
// from disclosure, and the audit records the cause explicitly through Cause.
func (c *ConfigFailure) Error() string {
	return fmt.Sprintf("%s (%s)", ErrConnectionUnusable.Error(), c.Stage)
}

// Is answers for the sentinel alone, so errors.Is recognises the class without
// reaching the cause.
func (c *ConfigFailure) Is(target error) bool { return target == ErrConnectionUnusable }

// AuditDetail is the operator's whole of it: the stage and the raw cause.
//
// THE CAUSE IS HERE AND NOWHERE ELSE ON THE WAY OUT. An audit row is read by
// someone who already has the connection row in front of them, so the DSN
// parser's complaint is exactly what they need; the same text on the wire
// would be this install's topology handed to whoever holds a socket.
func (c *ConfigFailure) AuditDetail() string {
	return fmt.Sprintf("stage=%s cause=%v", c.Stage, c.cause)
}

// Cause is the only way to the underlying error. Audit calls it; nothing on
// the wire path does.
func (c *ConfigFailure) Cause() error { return c.cause }

// ConfigFailureOf reports whether an error is one, and which.
func ConfigFailureOf(err error) (*ConfigFailure, bool) {
	var c *ConfigFailure
	if errors.As(err, &c) {
		return c, true
	}
	return nil, false
}
