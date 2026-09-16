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
	// ConfigStageUnclassified is what an unrecognised stage normalizes to. It
	// is a real answer -- this package could not attribute it -- rather than a
	// hole through which an arbitrary string reaches a projection.
	ConfigStageUnclassified ConfigStage = "unclassified"
)

// ConfigDetail is the WHOLE of what a configuration failure may say about
// itself outside this package. It is a closed set of fixed literals chosen at
// the raise site, never text formatted from a cause.
//
// A FORMATTED CAUSE WAS THE DEFECT. The audit detail used to be
// "stage=%s cause=%v", and the cause for a DSN failure is the driver's parser
// complaining about the DSN -- so the audit trail, and Event.Detail which is
// published more widely than a log, carried the estate's topology and its
// secrets. pgx redacts the password in a URL's userinfo and NOTHING else,
// which measured out as: the host leaked; a password passed as a query
// parameter leaked verbatim; and a PAT placed in the USERNAME position leaked
// whole, because the redactor only looks at the password field.
//
// A CLOSED SET RATHER THAN A SANITISER, deliberately. A sanitiser is a list of
// patterns that must keep up with every shape a secret can take, and the
// measurement above is what keeping up looks like when it fails. A raise site
// that must name one of these cannot leak something nobody thought of,
// because it never holds the cause in the first place.
type ConfigDetail string

const (
	// DetailUnknownEngine: the connection row names an engine not implemented.
	DetailUnknownEngine ConfigDetail = "the connection names an engine this build does not implement"
	// DetailDSNUnusable: the stored DSN did not survive the engine's own parser,
	// or sets an option that would desynchronize the statement classifier.
	DetailDSNUnusable ConfigDetail = "the stored connection string could not be used as written"
	// DetailPoolRefused: the driver's pool object would not construct.
	DetailPoolRefused ConfigDetail = "the driver would not build a connection pool for this connection"
	// DetailGrammarUnproved: the target's parsing mode could not be established.
	DetailGrammarUnproved ConfigDetail = "the target's statement parsing mode could not be established"
	// DetailNoDestroy: the resolved driver cannot destroy a pinned backend.
	DetailNoDestroy ConfigDetail = "the resolved driver cannot destroy a pinned backend on demand"
	// DetailStoreUnavailable: the secret store would not answer. Raised in the
	// front door rather than here, and declared here so the set stays closed.
	DetailStoreUnavailable ConfigDetail = "the secret store for this connection would not answer"
	// DetailUnclassified is what an unrecognised detail normalizes to.
	DetailUnclassified ConfigDetail = "this connection could not be used, and the reason was not classified"
)

// configDetails is every member of the closed set, for the walk that proves no
// raise site invents one.
func configDetails() []ConfigDetail {
	return []ConfigDetail{DetailUnknownEngine, DetailDSNUnusable, DetailPoolRefused,
		DetailGrammarUnproved, DetailNoDestroy, DetailStoreUnavailable, DetailUnclassified}
}

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
	// EVERY FIELD IS PRIVATE, AND THE CLOSED SET IS NOT ENOUGH WITHOUT THAT.
	//
	// ConfigStage and ConfigDetail are defined string types, so
	// ConfigDetail("<anything>") compiles anywhere. Exported fields therefore
	// let a caller outside this package inject arbitrary text into a
	// projection whose entire promise is that it carries only fixed literals,
	// and let any holder mutate a failure after the raise site had decided
	// what it says. The AST walk over raise sites cannot see either: it reads
	// constructor arguments in ONE package, and neither injection goes
	// through a constructor there.
	//
	// So the fields are private, the constructor normalizes, and the walk
	// stays as defence in depth rather than as the guarantee.
	stage  ConfigStage
	connID int64
	detail ConfigDetail
	cause  error
}

// Stage is which part of the configuration is wrong.
func (c *ConfigFailure) Stage() ConfigStage { return c.stage }

// ConnID is the connection's opaque numeric id. Safe to publish: a row number
// an operator looks up, never a name, a host or a credential.
func (c *ConfigFailure) ConnID() int64 { return c.connID }

// Detail is the fixed literal this failure is allowed to say about itself.
func (c *ConfigFailure) Detail() ConfigDetail { return c.detail }

// configStages is every stage this package may record.
func configStages() []ConfigStage {
	return []ConfigStage{ConfigStageEngine, ConfigStageDSN, ConfigStagePool,
		ConfigStageCapability, ConfigStageUnclassified}
}

// normalizeConfigStage and normalizeConfigDetail map anything not declared
// here onto the unclassified members.
//
// AN UNKNOWN VALUE IS NEVER PASSED THROUGH. A projection that promises fixed
// literals and then forwards whatever it was handed is not a projection, and
// "unclassified" is the honest answer for a value this package does not
// recognise -- the same answer the dial stages give.
func normalizeConfigStage(st ConfigStage) ConfigStage {
	for _, known := range configStages() {
		if st == known {
			return st
		}
	}
	return ConfigStageUnclassified
}

func normalizeConfigDetail(d ConfigDetail) ConfigDetail {
	for _, known := range configDetails() {
		if d == known {
			return d
		}
	}
	return DetailUnclassified
}

// NewConfigFailure builds one for a stage the caller already knows.
//
// THE STAGE IS ALWAYS PASSED, NEVER CLASSIFIED FROM THE CAUSE. Unlike a dial
// failure, whose cause arrives from a driver and has to be interpreted, every
// configuration failure is raised by a call site in this package that knows
// exactly which check it just failed. Inferring it would be guessing at
// something already known.
func NewConfigFailure(stage ConfigStage, connID int64, detail ConfigDetail, cause error) *ConfigFailure {
	return &ConfigFailure{stage: normalizeConfigStage(stage), connID: connID,
		detail: normalizeConfigDetail(detail), cause: cause}
}

// Error names the stage and nothing else. The cause is deliberately absent:
// this text reaches operator logs through paths that do not distinguish audit
// from disclosure. The audit does not record the cause either -- stage,
// connection id and the fixed literal are the durable facts, and Cause is
// in-process only.
func (c *ConfigFailure) Error() string {
	return fmt.Sprintf("%s (%s)", ErrConnectionUnusable.Error(), c.stage)
}

// SafeLog is what a log line may say. Same content as the audit row, because
// there is no second audience that has earned more: a log is copied into
// tickets, pasted into chat and shipped to aggregators.
func (c *ConfigFailure) SafeLog() string { return c.AuditDetail() }

// Is answers for the sentinel alone, so errors.Is recognises the class without
// reaching the cause.
func (c *ConfigFailure) Is(target error) bool { return target == ErrConnectionUnusable }

// AuditDetail is the operator's whole of it: the stage, the connection's
// opaque id, and the fixed literal the raise site chose.
//
// IT DOES NOT CARRY THE CAUSE, and that is the fix rather than an omission.
// An audit row is not a log line: Event.Detail is published to whatever
// consumes the event stream, which is wider than the operator sitting with the
// connection row in front of them. The three fields here are enough to find
// the row and know which check failed, and none of them can carry a secret.
// The cause stays private to this process; Cause exists for a caller that has
// already decided it is safe to look, and no path out of this package calls it.
func (c *ConfigFailure) AuditDetail() string {
	return fmt.Sprintf("stage=%s conn=%d detail=%s", c.stage, c.connID, c.detail)
}

// Cause is the only way to the underlying error. NOTHING ON THE WAY OUT OF
// THIS PROCESS CALLS IT -- not the wire, not the audit, not the log. It exists
// for a caller inside this package that has already established it is safe to
// look, and the cell that walks the raise sites is what keeps it that way.
func (c *ConfigFailure) Cause() error { return c.cause }

// ConfigFailureOf reports whether an error is one, and which.
func ConfigFailureOf(err error) (*ConfigFailure, bool) {
	var c *ConfigFailure
	if errors.As(err, &c) {
		return c, true
	}
	return nil, false
}
