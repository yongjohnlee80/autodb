package admission

// Mutation represents a data-modifying verb (INSERT, UPDATE, DELETE) identified
// within a statement, recording its parenthesis nesting depth and whether a
// WHERE predicate directly guards it at that exact depth.
//
// Nesting depth isolation is critical to preventing predicate confusion:
//
//	Depth 0: UPDATE users SET active = false WHERE id = 10;
//	         └───────── Depth 0 Verb ────────┘ └ Depth 0 WHERE (Guarded)
//
//	Depth 0: DELETE FROM users
//	Depth 1:   WHERE id IN (SELECT id FROM audit WHERE active = true);
//	                        └─ Depth 1 subquery ─┘ └ Depth 1 WHERE ─┘
//
// A WHERE clause inside a nested subquery (depth 1) NEVER protects the outer
// mutation (depth 0). The mutation guard requires an affirmative WHERE at
// depth 0.
type Mutation struct {
	Verb     string // uppercase, e.g. "DELETE"
	Depth    int    // the paren nesting depth the verb was found at
	HasWhere bool   // a WHERE at exactly that depth after the verb
}

// Call represents a function or procedure invocation identified anywhere in
// the statement text.
type Call struct {
	Name   string // bare name, as lexed
	Schema string // schema qualifier when written, "" otherwise
}

// FactClass is the statement authorization category, representing the
// maximum permission tier required by any clause within the statement:
//
//	ClassRead    (SELECT, EXPLAIN)
//	    ▲
//	ClassWrite   (INSERT, UPDATE, DELETE, MERGE)
//	    ▲
//	ClassDDL     (CREATE, ALTER, DROP, TRUNCATE, GRANT)
//	    ▲
//	ClassControl (BEGIN, COMMIT, ROLLBACK, SET, LOCK)
//
// For instance, a SELECT statement containing a data-modifying CTE is classified
// as ClassWrite.
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

// Facts exposes the structural properties of a SQL statement extracted by
// the lexer / parser frontend.
//
// The interface is read-only and decoupled from the engine's internal AST types:
//
//	+-------------------------------------------------------------------+
//	|                             Facts                                 |
//	+-------------------------------------------------------------------+
//	| Verb()             -> Main classified SQL verb (e.g. "UPDATE")    |
//	| Class()            -> Authorization category (Read/Write/DDL/Ctrl)|
//	| HasTopLevelWhere() -> True if WHERE clause exists at depth 0      |
//	| Mutations()        -> List of mutating verbs & nesting depths     |
//	| Calls()            -> List of function / routine invocation shapes|
//	| SetTarget()        -> GUC variable name, LOCAL flag, existence    |
//	| TextLen()          -> Total statement text length in bytes        |
//	+-------------------------------------------------------------------+
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
