package admission

// PhysicalCtx is the transport fact describing where and how the statement is running.
//
// It represents the physical execution surface rather than security policy:
//
//	+-----------------------------------------------------------------+
//	|                          PhysicalCtx                            |
//	+-----------------------------------------------------------------+
//	| PhysPooled:  Stateless pooled database backend.                 |
//	|              - No persistent wire connection.                   |
//	|              - Transactions pinned only during active TX block. |
//	+-----------------------------------------------------------------+
//	| PhysSession: Stateful local IPC / TUI session (unix socket).    |
//	|              - Interactive session lifecycle.                   |
//	|              - Session control verbs (SET, LOCK) permitted.     |
//	+-----------------------------------------------------------------+
//	| PhysWire:    Dedicated PostgreSQL wire session (TCP daemon).    |
//	|              - Pinned 1:1 to single backend for connection life.|
//	|              - Emulates native PostgreSQL wire protocol.        |
//	+-----------------------------------------------------------------+
type PhysicalCtx int

const (
	// PhysPooled: a stateless execution on a pooled connection. No wire
	// session exists; a transaction is pinned only when the caller's
	// session holds one open.
	PhysPooled PhysicalCtx = iota + 1

	// PhysSession: an RPC/TUI session — the autovim dbase over its unix
	// socket. First-class, not a shorter path: the SQL policy chain for a
	// connection is the same object, in the same order, as the front
	// door's.
	PhysSession

	// PhysWire: a front-door wire session pinned to one backend for its
	// whole life; the backend is discarded at close.
	PhysWire
)

// String names the physical context for chain renderings.
func (p PhysicalCtx) String() string {
	switch p {
	case PhysPooled:
		return "pooled"
	case PhysSession:
		return "session"
	case PhysWire:
		return "wire"
	}
	return "unknown"
}

// TargetCaps is the TARGET's capability set, supplied by the engine per
// connection: what the database itself can do. A typed bitset rather than
// a growing boolean per target fact, so a stage declares its requirement
// declaratively and the composition can make it absent by construction —
// a reader-analysis stage requiring a routine catalog is unsatisfiable on
// a target that has none, rather than discovering the absence in Apply and
// returning an empty contribution.
type TargetCaps uint

const (
	// CapRoutineCatalog: the target exposes a user-routine catalog the
	// reader analysis can consult (PostgreSQL-family targets).
	CapRoutineCatalog TargetCaps = 1 << iota

	// CapTxReadOnly: the target can host a server-enforced read-only
	// transaction.
	CapTxReadOnly
)

// Has reports whether every capability in want is present.
func (c TargetCaps) Has(want TargetCaps) bool { return c&want == want }

// String renders the set for diagnostics.
func (c TargetCaps) String() string {
	if c == 0 {
		return "none"
	}
	var parts []string
	if c&CapRoutineCatalog != 0 {
		parts = append(parts, "routine-catalog")
	}
	if c&CapTxReadOnly != 0 {
		parts = append(parts, "tx-readonly")
	}
	if len(parts) == 0 {
		return "unknown-bits"
	}
	return join(parts, "+")
}

// Needs declares what an admission stage requires from Context and Facts to be
// applicable.
//
// If any requirement is not satisfied by the runtime context or statement facts,
// the Orchestrator skips the stage entirely ("Absence by Construction"):
//
//	+-------------------------+      +--------------------------+
//	|     Stage.Needs         |  vs  |     Context & Facts      |
//	+-------------------------+      +--------------------------+
//	| OnSession: true         | <--- | PhysSession / PhysWire?  | -> (Mismatch: Skip)
//	| ReadOnlyUnit: true      | <--- | ctx.ReadOnly == true?    | -> (Mismatch: Skip)
//	| ControlVerb: true       | <--- | facts.Class() == Control | -> (Mismatch: Skip)
//	| SetShape: true          | <--- | facts.SetTarget() exists?| -> (Mismatch: Skip)
//	| TargetCaps: bits        | <--- | ctx.TargetCaps.Has(want) | -> (Mismatch: Skip)
//	+-------------------------+      +--------------------------+
//	                                              │
//	                                    All Needs Satisfied?
//	                                              │
//	                                              ▼
//	                                     Stage is APPLICABLE
type Needs struct {
	// OnSession: the stage requires an affirmative session-shaped physical
	// context (session or wire). Pooled — and any zero or invalid physical
	// context — does not satisfy it.
	OnSession bool

	// ReadOnlyUnit: the stage applies to reader units only (the reader
	// analysis).
	ReadOnlyUnit bool

	// ControlVerb: the stage applies to control-class statements only
	// (the SET/LOCK session-state gates).
	ControlVerb bool

	// SetShape: the stage requires a parsed set-statement shape in the
	// facts (the GUC stages). On a chain whose facts carry no set shape
	// the stage is unsatisfiable.
	SetShape bool

	// TargetCaps: the stage requires these target capabilities. A target
	// without them makes the stage absent by construction.
	TargetCaps TargetCaps
}

// Context is the dynamic half of a stage's input: the connection's
// capability profile, the physical context the statement arrived through,
// the execution-unit policy, and the session's transaction state. The
// engine builds one per evaluation; stages read it and never mutate it.
//
// Authority is deliberately NOT here. Authorization is resolved fresh at
// every Execute by the ENGINE — re-resolved, never cached — and its
// outcome reaches stages as Policy, a snapshot for THIS evaluation. A
// stage that wants to re-check between two Executes is handed a new
// Context built on a new policy; the interface gives it no way to hold the
// old one.
type Context struct {
	// Profile is the connection's capability profile name — a preset that
	// selects which stages compose the chain, never a branch inside a
	// shared function.
	Profile string

	// Phys is where the statement is running.
	Phys PhysicalCtx

	// ReadOnly is the unit's read-only policy: the unit requires a
	// server-enforced read-only transaction.
	ReadOnly bool

	// MayWrite is the unit's write floor.
	MayWrite bool

	// TxOpen reports whether the caller's transaction is open. Session
	// state, not policy.
	TxOpen bool

	// Aborted reports a failed transaction (recovery controls only).
	Aborted bool

	// PinnedTx reports whether the caller's execution carries a pinned
	// transaction — the legacy onSession fact, carried as its OWN field
	// and deliberately independent of the physical transport. A pooled
	// stateless call with a session-held transaction pinned is STILL
	// PhysPooled (transport applicability must not be corrupted), but the
	// session profile admits its control verbs inside that transaction,
	// exactly as the legacy gate's pinned != nil answer did. Relabelling
	// the call as a session would let OnSession-applicable stages run on
	// a pooled connection; the separate field keeps the two facts two
	// facts.
	PinnedTx bool

	// TargetCaps is what the connection's target can do, supplied by the
	// engine per connection. The zero value means NO capabilities — a
	// stage requiring any capability is unsatisfiable against it.
	TargetCaps TargetCaps

	// MaxStatementBytes is the intake bound the size stage enforces.
	MaxStatementBytes int
}

// join is strings.Join without importing strings — the leaf keeps its
// import set to the stdlib's most basic surface deliberately.
func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
