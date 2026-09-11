package admission

// Mutation is one data-modifying verb found below top level, with the guard
// input for that verb at ITS OWN depth — a WHERE belonging to an inner
// subquery is not a guard on the mutation that encloses it.
type Mutation struct {
	Verb     string // uppercase, e.g. "DELETE"
	Depth    int    // the paren nesting depth the verb was found at
	HasWhere bool   // a WHERE at exactly that depth after the verb
}

// Call is one function-call shape found anywhere in the statement, with its
// schema qualifier when written. Keywords that happen to precede a paren
// appear here too; they are harmless because no user-defined function can
// be called by an unquoted keyword.
type Call struct {
	Name   string // bare name, as lexed
	Schema string // schema qualifier when written, "" otherwise
}

// FactClass is the authorization class: the MAXIMUM class of any verb in
// the statement, so a read whose CTE body writes is authorized as a write.
// Whether such a statement then EXECUTES is a stage's decision, not this
// field's.
type FactClass string

const (
	// ClassRead is SELECT-shaped.
	ClassRead FactClass = "read"
	// ClassWrite is DML.
	ClassWrite FactClass = "write"
	// ClassDDL is schema or privilege change.
	ClassDDL FactClass = "ddl"
	// ClassControl is transaction/session control — BEGIN, SET, LOCK and
	// kin. The classifier reports what the statement IS; admission decides
	// whether it may run, which is why this is a fact and not a verdict.
	ClassControl FactClass = "control"
)

// HasTopLevelWhere reports a mutation guard's depth-0 input: a WHERE at
// paren depth 0 after the main verb.
//
// Fact applicability is part of this interface: a stage declares which
// facts it requires PRESENT, so a stage that needs a set-statement shape
// is unsatisfiable on a chain whose facts carry none — absent by
// construction, not by discipline. The optionals below return the zero
// value and a false; a stage that never asks cannot be surprised by one
// that is missing.
//
// LIFECYCLE SCOPE IS DELIBERATELY NOT HERE YET. The eventual contract —
// per-fact provenance, per-stage scope requirements, per-Execute
// recomputation — is introduced with the first drive step that recomputes
// and asserts it, where it is enforced by the lifecycle mutation cells,
// rather than frozen here as an unused scalar that claims a model the
// code does not implement.
type Facts interface {
	// Verb is the classified main verb, uppercase.
	Verb() string

	// Class is the statement's authorization class.
	Class() FactClass

	// HasTopLevelWhere reports a top-level WHERE after a main
	// UPDATE/DELETE verb.
	HasTopLevelWhere() bool

	// Mutations are the data-modifying verbs found below top level, at
	// their own depths.
	Mutations() []Mutation

	// Calls are the function-call shapes found in the statement.
	Calls() []Call

	// SetTarget reports the GUC a SET names: the target setting, and
	// whether a parsed set-statement shape exists at all. A stage that
	// guards SET grammar asks; every other stage ignores.
	SetTarget() (name string, local bool, exists bool)

	// TextLen is the statement text's length, for the intake bound.
	TextLen() int
}
