package admission

// Code is a refusal's rule identity: protocol-neutral, stable, and the
// thing a surface RENDERS from rather than switches on. The minimal deny
// set plus the context-dependent identities that keep their own codes —
// each is the identity the existing sentinel carries, so an adapter's
// mapping table is a renaming, not a redefinition.
type Code string

// RefusalClass is the protocol-neutral recovery class a surface maps onto its
// own status vocabulary.
type RefusalClass uint8

const (
	ClassPermission RefusalClass = iota + 1
	ClassUnsupported
	ClassProgramLimit
)

const (
	// CodeStatementUnsupported: the statement class is not supported
	// through the engine on this profile — control verbs off a session,
	// data-modifying CTEs under the compat profile, unclassifiable
	// leading tokens.
	CodeStatementUnsupported Code = "statement-unsupported"

	// CodeScriptTooLarge: the text exceeds the intake bound before
	// classification, so the audit record equals what ran.
	CodeScriptTooLarge Code = "script-too-large"

	// CodeNoWhere: a mutation that can reach every row must say which
	// rows it means.
	CodeNoWhere Code = "mutation-without-predicate"

	// CodeReaderAdvancedPattern: a reader unit invoked user-defined code
	// or a procedural block — the editors-first rule.
	CodeReaderAdvancedPattern Code = "reader-advanced-pattern"

	// CodeSetGUCRefused: a SET names a setting the session-state gate
	// refuses (engine belt, grammar-changing, or off the allowlist).
	CodeSetGUCRefused Code = "set-guc-refused"

	// CodeSetNotLocal: a SET without LOCAL would persist onto a pooled
	// connection beyond the caller's transaction.
	CodeSetNotLocal Code = "set-not-local"

	// CodeSetOutsideTx: a SET LOCAL outside a transaction has no boundary
	// to revert at.
	CodeSetOutsideTx Code = "set-outside-tx"

	// CodeLockOutsideTx: a LOCK outside a transaction is released
	// immediately — admitting it would leave the caller believing they
	// hold one they do not.
	CodeLockOutsideTx Code = "lock-outside-tx"

	// CodeWireSetRefused: the wire session-state denylist (parsing GUCs,
	// authority-in-disguise SET forms, reader search_path).
	CodeWireSetRefused Code = "wire-set-refused"

	// CodeDenied: the authorization floor — uniform, never disclosing
	// existence.
	CodeDenied Code = "denied"

	// CodeReadOnlyUnenforceable: a read-only unit on a target that cannot
	// host a read-only transaction, where the promise was made.
	CodeReadOnlyUnenforceable Code = "read-only-unenforceable"

	// CodeGrammarDrifted refuses a pinned session whose target parsing mode no
	// longer matches the mode under which autodb classifies SQL.
	CodeGrammarDrifted Code = "grammar-drifted"
)

// Reason is ONE refusal's protocol-neutral identity: what was refused, on
// which rule, where in the text, and whether the session survives it. The
// surface renders this into ITS OWN vocabulary — the front door into a
// SQLSTATE with the code as the rule id and the span as position, the RPC
// into its error shape — so adding a rule must not require editing the
// core or the surface.
type Reason struct {
	// Code is the rule identity. The surface NEVER switches on it; it
	// carries it.
	Code Code

	// Class lets a surface select its status family without knowing the rule.
	Class RefusalClass

	// Span is the refusal's position in the statement text, 1-based, or
	// 0 when the refusal is not positional.
	Span int

	// Subject names what was refused — the verb, the setting, the call.
	Subject string

	// Detail is the human-readable explanation, already composed by the
	// stage from the rule's own vocabulary. A refusal returned as an error
	// would re-merge policy with operational failure; this field keeps
	// the explanation without the conflation.
	Detail string

	// Legacy preserves an established Go errors.Is identity while old callers
	// migrate to the structured Reason. It is compatibility metadata, never
	// rendered or interpreted as policy; new analyzer rules leave it nil.
	Legacy error

	// Hint is an actionable recovery note when the rule has one.
	Hint string

	// Continue reports whether the SESSION remains usable after this
	// refusal — an ERROR, not a FATAL. Whether a locked store leaves a
	// session usable is a judgement about the RULE, so it travels with
	// the reason and the surface only turns it into protocol severity.
	Continue bool
}

// Observation is a risk finding: a statement that was NOT denied but whose
// shape carries risk worth recording and trending. Arms attach to the
// statement's disposition record either way; a rising risk rate is a
// signal rather than a silence.
type Observation struct {
	Code   Code
	Detail string
}

// Contribution is what one stage may say about one statement: a deny
// Reason, a risk Observation, or nothing. It is a STRUCT, not an error —
// a stage has no way to refuse outside these arms, which is what keeps the
// pipeline's refusal surface closed. The orchestrator reads Deny only
// when deciding whether to dispatch; Risk never gates.
type Contribution struct {
	Deny *Reason
	Risk *Observation
}

// Deny builds a denying contribution.
func Deny(r Reason) Contribution { return Contribution{Deny: &r} }

// Risk builds an observing contribution.
func Risk(o Observation) Contribution { return Contribution{Risk: &o} }

// NoContribution is the explicit empty verdict.
func NoContribution() Contribution { return Contribution{} }

// Stage is one admission step: a check composed into a chain, applicable
// or not by construction, and permitted exactly one refusal currency.
//
// A stage's Apply returns a Contribution and an error, and the two answer
// DIFFERENT questions. The Contribution is policy: what is wrong with the
// statement. The error is operational: the STAGE broke — a catalog read
// failed, a resolver is unreachable. An error is never a refusal, and a
// refusal is never an error; conflating them is the failure the split
// exists to prevent, because "the analyzer could not read the catalog"
// and "the statement is refused" must reach the client differently.
//
// DISCLOSURE IS MANDATORY. DenyCodes declares every Code the stage can
// deny with — nil for a stage that never denies. The registration walks
// (the renderer-completeness check, the record-on-every-refusal
// enumeration) derive their obligations from these declarations, so a
// denying stage with an undeclared code is a refusal the pipeline's
// consumers cannot know about: the orchestrator REJECTS a stage whose
// Apply denies with a code it did not declare, loudly, at evaluation time.
// An optional interface would let a denying stage evade every downstream
// obligation while running normally — the central claim of the seam is
// that it cannot.
type Stage interface {
	// Name is the stage's stable identity, used in chain renderings and
	// order assertions.
	Name() string

	// ContextNeeds declares what the stage requires to be applicable. A
	// chain composition that cannot supply the declared needs makes the
	// stage ABSENT by construction — inapplicable, not silently empty.
	ContextNeeds() Needs

	// DenyCodes declares every Code this stage can deny with; nil for a
	// pure observer. The disclosure is the obligation: the orchestrator
	// rejects an undeclared denial rather than letting it run.
	DenyCodes() []Code

	// Apply evaluates one statement's facts in one context.
	Apply(Facts, Context) (Contribution, error)
}

// Analyzer is the producer interface: one implementation of the whole
// evaluation rather than one step of it. The legacy guards adapt as Stages
// today (the behaviour-preserving phase); the lexer/AST risk analyzer
// implements THIS tomorrow — the same chain, the same Report, a second
// implementation the shadow differential can compare against.
type Analyzer interface {
	// Analyze evaluates a statement wholesale, returning the full report.
	Analyze(Facts, Context) (Report, error)
}

// Report is the evaluation's DATA: what the pipeline concluded, with every
// arm a consumer may read. A struct, not an interface — arms are added by
// adding fields and nobody breaks. IsDenied reads Deny only, never Risk,
// never a threshold: the predicate stays on the gate.
type Report struct {
	// Deny is non-empty when the statement is refused. The FIRST reason
	// is the fundamental one — the chain refuses on the most fundamental
	// ground first, so a caller fixing it learns the real blocker, not a
	// symptom.
	Deny []Reason

	// Risk carries observations for admitted statements.
	Risk []Observation
}

// IsDenied reports whether the evaluation refused the statement.
func (r Report) IsDenied() bool { return len(r.Deny) > 0 }

// PrimaryDeny returns the fundamental refusal, if any.
func (r Report) PrimaryDeny() (Reason, bool) {
	if len(r.Deny) == 0 {
		return Reason{}, false
	}
	return r.Deny[0], true
}
