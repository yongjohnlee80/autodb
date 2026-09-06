package engine

// What an engine can do, asked as the question the caller actually has.
//
// THE DEFECT THIS REPLACES. Forty-two places compared an engine to a constant
// in order to decide something that was never about identity. A reconciler
// asked `!= Postgres` when its question was "can this target tell me whether a
// commit landed"; a timeout armed a server-side belt behind `!= Postgres` when
// its question was "is there a server-side statement timeout to arm"; four
// call sites computed `== MySQL` to fill a parameter whose own name is
// `backslashEscapes`. The callee named the concern correctly and every caller
// restated it as an identity.
//
// That is not merely indirect. A predicate standing in for a concern drifts
// from it: the day an engine gains a commit-status oracle, `!= Postgres` is
// still true of it and the reconciler still refuses to ask. The identity is
// evidence for the capability, not the capability, and the two part company
// exactly when a third engine arrives — which is the moment this codebase is
// meant to be ready for.
//
// SIX PREDICATES ANSWER `n == Postgres` TODAY. That is a coincidence of the
// three engines currently supported, not one fact spelled six ways, and they
// are deliberately NOT collapsed. Merging them would compile and would be
// wrong in the way the outcome vocabularies were wrong: the same answer about
// different subjects. The tell is what a fourth engine does to them — a
// CockroachDB target has a commit-status oracle and declarative partitioning
// but is not reached by the pgx raw-wire relay, and the merged predicate would
// have no way to say so. See docs/reference/duplication-residue.md.

// capabilities is one engine's row. Fields rather than methods on a per-engine
// type so that adding an engine is filling in a row the compiler already
// demands, and adding a capability is a field every existing row must answer.
type capabilities struct {
	// backslashEscapes: a backslash inside a single-quoted string literal
	// escapes the next character, rather than being an ordinary backslash.
	// The lexer's behaviour depends on it, so a wrong answer here mis-reads
	// where a statement ends — which is a classification failure, not a
	// cosmetic one.
	backslashEscapes bool

	// grammarVerifiedPerConnection: the driver offers a per-physical-connection
	// seam at which the session's grammar settings can be verified once. Where
	// it does not, every statement must carry its own verification inside a
	// transaction, which is why the caller's question is about the seam and not
	// about the engine.
	grammarVerifiedPerConnection bool

	// commitStatusOracle: the target can be asked, after the fact, whether a
	// transaction committed. Without one an unanswered commit is not merely
	// unproven now but unprovable ever, which is a terminal condition rather
	// than a retryable one.
	commitStatusOracle bool

	// reportsTransactionID: the target exposes its own transaction id while the
	// transaction is open. The recovery reconciler is useless without it — it
	// is the handle the oracle is later asked about.
	reportsTransactionID bool

	// serverStatementTimeout: the server enforces a statement deadline of its
	// own, so the client's deadline can be backed by a second layer that
	// survives a client that stops reading.
	serverStatementTimeout bool

	// declarativePartitioning: the store can be partitioned by the engine
	// itself, so the volume tables can be rolled rather than pruned row by row.
	declarativePartitioning bool

	// routineCatalog: the target has a catalog of callable routines in the
	// shape the reader analysis queries. Where it does not, reader safety
	// rests on the classifier and the driver's read-only transaction.
	routineCatalog bool

	// postgresWire: the target speaks the PostgreSQL wire protocol, so a
	// client's frames can be relayed onto it natively. Approximating the
	// extended protocol on an engine that does not — decoding frames and
	// re-issuing them as ordinary statements — would silently drop the
	// guarantees the client asked for by using it.
	postgresWire bool
}

// The table. Every Name declared in this package must appear here, and
// TestEveryEngineHasCapabilities is what makes that true rather than intended:
// a missing row is not a compile error, it is a lookup that returns the zero
// value, and a zero value here reads as "this engine can do nothing" — which
// for backslashEscapes is not the safe direction but a silent claim about the
// grammar.
var capsByName = map[Name]capabilities{
	Postgres: {
		backslashEscapes:             false,
		grammarVerifiedPerConnection: true,
		commitStatusOracle:           true,
		reportsTransactionID:         true,
		serverStatementTimeout:       true,
		declarativePartitioning:      true,
		routineCatalog:               true,
		postgresWire:                 true,
	},
	MySQL: {
		backslashEscapes: true,
		// No per-connect seam in database/sql, so each statement verifies its
		// own grammar inside a transaction.
		grammarVerifiedPerConnection: false,
		commitStatusOracle:           false,
		reportsTransactionID:         false,
		serverStatementTimeout:       false,
		declarativePartitioning:      false,
		routineCatalog:               false,
		postgresWire:                 false,
	},
	SQLite: {
		backslashEscapes: false,
		// Its grammar is fixed: there is nothing a session can change and so
		// nothing to verify per statement.
		grammarVerifiedPerConnection: true,
		commitStatusOracle:           false,
		reportsTransactionID:         false,
		serverStatementTimeout:       false,
		declarativePartitioning:      false,
		routineCatalog:               false,
		postgresWire:                 false,
	},
}

// BackslashEscapes reports whether a backslash escapes the next character
// inside a single-quoted string literal.
//
// The classifier takes this as a parameter of the same name; the four call
// sites that used to compute `engine == MySQL` to fill it were each restating
// the engine's grammar as the engine's identity.
func (n Name) BackslashEscapes() bool { return capsByName[n].backslashEscapes }

// VerifiesGrammarPerConnection reports whether the driver offers a
// per-physical-connection seam at which grammar settings can be verified once.
//
// Where it is false, statements run inside a transaction that verifies first —
// which costs a round trip and, more importantly, makes DDL that a transaction
// prohibits unrunnable, so this is a question worth asking by name.
func (n Name) VerifiesGrammarPerConnection() bool {
	return capsByName[n].grammarVerifiedPerConnection
}

// HasCommitStatusOracle reports whether the target can be asked, after the
// fact, whether a transaction committed.
func (n Name) HasCommitStatusOracle() bool { return capsByName[n].commitStatusOracle }

// ReportsTransactionID reports whether the target exposes its own transaction
// id while the transaction is open.
//
// NOT the same question as HasCommitStatusOracle, though both are true of
// exactly one engine today. One is "can I get a handle", the other is "can I
// ask about a handle later", and an engine could plausibly do the first
// without the second.
func (n Name) ReportsTransactionID() bool { return capsByName[n].reportsTransactionID }

// HasServerStatementTimeout reports whether the server enforces a statement
// deadline of its own.
func (n Name) HasServerStatementTimeout() bool { return capsByName[n].serverStatementTimeout }

// SupportsDeclarativePartitioning reports whether the engine can partition a
// table itself.
func (n Name) SupportsDeclarativePartitioning() bool {
	return capsByName[n].declarativePartitioning
}

// HasRoutineCatalog reports whether the target has a catalog of callable
// routines in the shape the reader analysis queries.
func (n Name) HasRoutineCatalog() bool { return capsByName[n].routineCatalog }

// SpeaksPostgresWire reports whether a client's protocol frames can be relayed
// onto the target natively.
func (n Name) SpeaksPostgresWire() bool { return capsByName[n].postgresWire }
