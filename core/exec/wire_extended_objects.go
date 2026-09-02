package exec

import (
	"errors"

	"github.com/yongjohnlee80/golib/dao"
)

// THE EXTENDED-PROTOCOL OBJECT STORE (F2, matrix §4a).
//
// A session's wire-level prepared statements and portals, with PostgreSQL's own
// lifetimes. It lives engine-side on purpose: an extended segment spans many
// frames and every rule below is stateful, so the front-door loop forwards
// frames and this store owns what they mean. A loop that decided object fates
// would be a second state machine holding half the truth.
//
// Wire-level prepared statements are NOT SQL-text `PREPARE`. The classifier
// never sees a PREPARE verb here: Parse carries the statement text itself, which
// is exactly what lets F2 admit prepared statements while the profile keeps
// refusing the verb.

var (
	// ErrDuplicateStatement is a Parse naming a live NAMED statement.
	// PostgreSQL answers 42P05; the name must be closed before it is reused.
	// The unnamed statement is exempt — naming it is an implicit replacement.
	ErrDuplicateStatement = errors.New("exec: prepared statement already exists")

	// ErrRetainedBudget is a Parse or Bind the session's retained-state budget
	// cannot admit (matrix §7 :381 — 53400 frontdoor/retained-budget).
	//
	// Refused BEFORE the frame is forwarded: the target must never hold a
	// server-side prepared statement the budget did not admit, and a refusal
	// after the fact would be a budget reporting on an object that already
	// exists.
	ErrRetainedBudget = errors.New("exec: the session's retained-state budget cannot admit this object")

	// ErrNamedObjectCap is a Parse or Bind beyond §9's per-session namespace
	// limits (matrix §7 :385 — 53400 frontdoor/named-object-cap).
	ErrNamedObjectCap = errors.New("exec: the session holds too many named prepared statements or portals")

	// ErrParamCap is a Bind carrying more parameters than §9 admits (matrix
	// §7 :384 — 54000 frontdoor/param-cap). A PROGRAM limit, not a configured
	// quota: it bounds what one frame may make the front door pre-allocate, and
	// no operator setting raises it.
	ErrParamCap = errors.New("exec: the Bind carries more parameters than the front door admits")

	// ErrUnknownStatement is a Bind/Describe/Close naming a statement that does
	// not exist (PostgreSQL 26000).
	ErrUnknownStatement = errors.New("exec: prepared statement does not exist")

	// ErrDuplicatePortal is a Bind naming a live NAMED portal (PostgreSQL
	// 42P03). The unnamed portal is exempt, for the same reason as above.
	ErrDuplicatePortal = errors.New("exec: portal already exists")

	// ErrUnknownPortal is an Execute/Describe/Close naming a portal that does
	// not exist (PostgreSQL 34000).
	ErrUnknownPortal = errors.New("exec: portal does not exist")
)

// extStatement is one wire-level prepared statement.
type extStatement struct {
	name string
	sql  string

	// stmt is the classification decided ONCE, at Parse, and never re-derived.
	// Statement is a value type, so holding it is structurally immutable — a
	// later Execute cannot reclassify the text, which is the whole point of
	// gating at Parse (ADR-0075 §5: "immutable classification/guard metadata
	// attached to the statement").
	//
	// AUTHORITY IS DELIBERATELY ABSENT. Nothing about who may run this is
	// stored: the policy is re-resolved at every Execute (§5 rev 2 MF1, task
	// rejection criterion 3). Caching a verdict here is precisely the defect
	// that criterion exists to catch — a grant revoked between Parse and
	// Execute must refuse, and it cannot if the answer was frozen at Parse.
	stmt Statement

	// paramOIDs are the client's declared parameter types, kept verbatim so the
	// relay forwards what the client sent rather than a re-derivation.
	paramOIDs []uint32

	// portals names every live portal constructed from this statement, for
	// §4a's protocol-documented Close-S cascade.
	portals map[string]struct{}

	// seq is this object's GENERATION. See extObjects.nextSeq.
	seq uint64

	// charge and finalized are this object's retained accounting. See
	// extObjects.retained for the whole model.
	charge    int64
	finalized bool
}

// extPortal is one portal: a statement bound to parameters, awaiting Execute.
type extPortal struct {
	name string

	// stmtName links the portal to its statement by NAME rather than by
	// pointer, so a cascade cannot leave a portal addressing a statement the
	// store has already dropped.
	stmtName string

	// suspended records that a previous Execute returned PortalSuspended, so a
	// resuming Execute is a continuation rather than a fresh one. The
	// distinction matters for the audit: every Execute is re-authorized, and a
	// resumption is still an Execute (§5 rev 2 MF1 says "portal re-executions
	// included").
	suspended bool

	// seq is this object's GENERATION. See extObjects.nextSeq.
	seq uint64

	// charge and finalized are this object's retained accounting.
	charge    int64
	finalized bool
}

// extObjects is one session's extended-protocol namespace and the segment's
// outstanding-frame count.
//
// Not independently locked: it is reached only under the session's one
// in-flight claim, which is the same serialization every other per-session
// structure here relies on. A second lock would suggest a concurrency this
// protocol does not have.
type extObjects struct {
	statements map[string]*extStatement
	portals    map[string]*extPortal

	// segment is the ORDER in which this segment's queued frames will be
	// answered, one step per frame.
	//
	// Two things force an ordered list rather than a counter. First, a Flush does
	// not delimit one frame's answer: the server processes queued frames IN ORDER
	// and one Flush makes it emit the responses for ALL of them, so a client that
	// pipelines Parse, Bind and Execute gets three answers from one Flush and a
	// reader stopping at the first would abandon the result. Second, owned
	// transaction control never reaches the wire at all (ADR-0075 Amendment 6
	// ruling: control routes to the session's machine and is answered with the
	// protocol's fixed frames), so some steps are answered locally and some from
	// the connection — and the client must receive them in the order it asked.
	segment []segStep

	// roWrap is the hidden READ ONLY transaction a reader's segment runs inside.
	//
	// F3a's guarantee is that the SERVER enforces what the classifier decided: a
	// write smuggled through a volatile function classifies as a read, passes
	// every gate autodb has, reaches the target — and PostgreSQL refuses it with
	// 25006 because the unit is running READ ONLY. The raw path opens exactly
	// this wrap (wire_query.go), and the extended path has to open it too or the
	// guarantee holds on one protocol and not the other.
	//
	// It is a property of the SEGMENT, not of one frame, because golib requires
	// the quiescent state to begin a transaction and the wire stops being
	// quiescent the moment the first frame is queued. So it is opened when a
	// segment starts and rolled back when Sync ends it.
	roWrap dao.ContextTxConn

	// nextSeq generates each object's GENERATION.
	//
	// A NAME IS NOT AN IDENTITY. The unnamed statement can be replaced while the
	// first Parse's answer is still in flight, so a rule that matches a frame to
	// an object by name alone attributes the first Parse's answer to the
	// REPLACEMENT. The generation is what makes "this frame belongs to that
	// object" answerable at all — and the extended path's outcome attribution
	// (wire_extended.go) is decided by exactly that question.
	nextSeq uint64

	// retained and retainedPending are the session's retained-state account.
	// Pending is reserved but not yet confirmed by the target; retained is
	// confirmed. Both count against the same budget, because a reservation the
	// target has not answered yet is still memory held for it.
	retained, retainedPending int64

	// lateCompletions counts completions that arrived for an object the store no
	// longer holds. Diagnostic only — see finalizeRetained for why it must not
	// release.
	lateCompletions int
}

// segStep is one queued frame's place in the reply order. A step with synth
// frames is answered by the front door; an empty one is answered by the server.
type segStep struct {
	synth []WireMessage

	// obj names the object this frame CREATES, when it creates one. Nil for
	// frames that create nothing (Describe, Execute, Close).
	//
	// The identity rides the step the drain already walks, rather than arriving
	// through a channel or a second registry: the drain observes the answer, so
	// the drain is where the owner is known.
	obj *objectRef

	// exec marks the Execute frame of the call that queued it.
	//
	// WITHOUT THIS THE OWNER OF A nil-obj FRAME IS AMBIGUOUS: Describe, Close and
	// Execute all create nothing, so an error on one of them could only be
	// attributed by POSITION in the segment — and position is precisely the rule
	// that gets `SELECT 1/0` wrong, since the constant folds at plan time and the
	// target raises 22012 at Bind rather than at Execute. Marking the frame is
	// how the attribution stays a question about identity.
	exec bool
}

// objectRef identifies one object in the store's key space: kind, name AND
// generation. All three, for the reason given at extObjects.nextSeq.
type objectRef struct {
	kind objectKind
	name string
	seq  uint64
}

// objectKind separates the two namespaces §4a keeps apart: a statement and a
// portal may share a name and are still different objects.
type objectKind int

const (
	objectStatement objectKind = iota
	objectPortal
)

// sameObject reports whether two refs name the same object, generation included.
func (r objectRef) sameObject(other objectRef) bool {
	return r.kind == other.kind && r.name == other.name && r.seq == other.seq
}

// queueWire records a frame that went to the connection and whose answer must be
// read back from it.
func (o *extObjects) queueWire() { o.segment = append(o.segment, segStep{}) }

// queueWireFor records a frame that CREATES an object, so the drain can tell
// whose answer it is reading.
func (o *extObjects) queueWireFor(kind objectKind, name string, seq uint64) {
	// The generation is captured HERE, at queue time, so the step names the
	// object this frame created rather than whatever holds the name later.
	o.segment = append(o.segment, segStep{obj: &objectRef{kind: kind, name: name, seq: seq}})
}

// queueExec records the Execute frame of the call that queued it. See
// segStep.exec for why the Execute is marked rather than inferred.
func (o *extObjects) queueExec() { o.segment = append(o.segment, segStep{exec: true}) }

// queueSynth records a frame the front door answers itself, with the fixed
// shapes the protocol defines for it.
func (o *extObjects) queueSynth(msgs ...WireMessage) {
	o.segment = append(o.segment, segStep{synth: msgs})
}

// queueSynthFor is queueSynth for a frame that CREATES an object.
//
// Owned control never reaches the target, so its ParseComplete and BindComplete
// are ours to send — and an object whose completion the front door produces
// still has to be finalized (lector B r0 MF2). Without the reference its
// reservation stays pending forever and Sync sweeps it, destroying a control
// statement the session is still entitled to use.
func (o *extObjects) queueSynthFor(kind objectKind, name string, seq uint64, msgs ...WireMessage) {
	o.segment = append(o.segment, segStep{
		synth: msgs,
		obj:   &objectRef{kind: kind, name: name, seq: seq},
	})
}

func newExtObjects() *extObjects {
	return &extObjects{
		statements: make(map[string]*extStatement),
		portals:    make(map[string]*extPortal),
	}
}

// putStatement records a parsed statement, applying §4a's replacement rule.
//
// The unnamed statement is implicitly replaced; a named one must be closed
// first. Replacement cascades, because the portals of the statement that is
// going away cannot outlive it.
func (o *extObjects) putStatement(st *extStatement) error {
	if st.name != "" {
		if _, live := o.statements[st.name]; live {
			return ErrDuplicateStatement
		}
	}
	// ADMISSION IS ATOMIC (lector B r0 MF3). The unnamed statement is REPLACED,
	// and the old one used to be destroyed first — so a budget refusal left the
	// store having forgotten a statement the TARGET still holds, and a later
	// Describe failed here for an object that exists there.
	//
	// Reserving first means a replacement is measured while the old object is
	// still counted, so one at the very edge of the budget can be refused where
	// releasing first would have fitted it. That is the safe direction: a refusal
	// is recoverable — Close and retry — and forgetting an object the target
	// holds is not.
	// THE NAMESPACE CAP, then the memory budget — both before the frame is
	// forwarded. A refusal here leaves the connection usable (§7 :385); it is the
	// Parse that is refused, not the session.
	if st.name != "" && o.namedStatements() >= maxNamedStatements {
		return ErrNamedObjectCap
	}
	// RESERVED BEFORE THE FRAME IS FORWARDED. The budget decides before the
	// Parse goes out, never after: the target must never hold a server-side
	// prepared statement the budget did not admit.
	if err := o.reserveRetained(st.charge); err != nil {
		return err
	}
	if st.name == "" {
		if _, live := o.statements[""]; live {
			o.dropStatement("")
		}
	}
	st.portals = make(map[string]struct{})
	// The generation is stamped on ADMISSION, so a replacement of the same name
	// is a different object from the moment it exists.
	o.nextSeq++
	st.seq = o.nextSeq
	o.statements[st.name] = st
	return nil
}

// statement returns a live statement by name.
func (o *extObjects) statement(name string) (*extStatement, error) {
	st, ok := o.statements[name]
	if !ok {
		return nil, ErrUnknownStatement
	}
	return st, nil
}

// putPortal records a bound portal, applying §4a's replacement rule and
// registering it for its statement's cascade.
func (o *extObjects) putPortal(p *extPortal) error {
	st, err := o.statement(p.stmtName)
	if err != nil {
		return err
	}
	if p.name != "" {
		if _, live := o.portals[p.name]; live {
			return ErrDuplicatePortal
		}
	}
	if p.name != "" && o.namedPortals() >= maxNamedPortals {
		return ErrNamedObjectCap
	}
	if err := o.reserveRetained(p.charge); err != nil {
		return err
	}
	if p.name == "" {
		if _, live := o.portals[""]; live {
			o.dropPortal("")
		}
	}
	o.nextSeq++
	p.seq = o.nextSeq
	o.portals[p.name] = p
	st.portals[p.name] = struct{}{}
	return nil
}

// portal returns a live portal by name.
func (o *extObjects) portal(name string) (*extPortal, error) {
	p, ok := o.portals[name]
	if !ok {
		return nil, ErrUnknownPortal
	}
	return p, nil
}

// dropStatement releases a statement AND every portal built from it — the
// cascade §4a calls protocol-documented. Reported so a caller can tell a real
// close from a no-op on a name that was never there.
func (o *extObjects) dropStatement(name string) bool {
	st, ok := o.statements[name]
	if !ok {
		return false
	}
	// THE DROP OWNS THE CHARGE, pending or finalized — every §4a release point
	// does. A completion arriving afterwards is a no-op; releasing in both places
	// would hand out capacity that does not exist.
	for portalName := range st.portals {
		if p, ok := o.portals[portalName]; ok {
			o.releaseRetained(p.charge, p.finalized)
			delete(o.portals, portalName)
		}
	}
	o.releaseRetained(st.charge, st.finalized)
	delete(o.statements, name)
	return true
}

// dropPortal releases one portal and unregisters it from its statement, so the
// statement's cascade set does not accumulate names of portals already gone.
func (o *extObjects) dropPortal(name string) bool {
	p, ok := o.portals[name]
	if !ok {
		return false
	}
	if st, ok := o.statements[p.stmtName]; ok {
		delete(st.portals, name)
	}
	o.releaseRetained(p.charge, p.finalized)
	delete(o.portals, name)
	return true
}

// dropAllPortals releases every portal, named and unnamed, and is §4a's
// transaction-end rule: portals do not survive the transaction, prepared
// statements do.
//
// It is called on COMMIT and ROLLBACK alike, including an implicit block ending
// and a failed transaction recovering, because PostgreSQL destroys portals on
// all of those and a portal that outlived its transaction would execute against
// a snapshot that no longer exists.
func (o *extObjects) dropAllPortals() {
	for _, st := range o.statements {
		st.portals = make(map[string]struct{})
	}
	for _, p := range o.portals {
		o.releaseRetained(p.charge, p.finalized)
	}
	o.portals = make(map[string]*extPortal)
}

// dropUnnamed releases the unnamed statement and the unnamed portal, which is
// §4a's simple-`Query` rule: a Query destroys both, and a client that mixes the
// two protocols (lib/pq does — it sends simple for parameterless statements)
// depends on that being true here as well.
func (o *extObjects) dropUnnamed() {
	o.dropPortal("")
	o.dropStatement("")
}

// THE SESSION'S RETAINED-STATE ACCOUNT (F2b, matrix §8 :411 and §7 :381).
//
// Three phases, and the order is the whole point (r0 MF3): a charge is RESERVED
// before the Parse/Bind is forwarded, FINALIZED when the target's completion
// arrives, and RELEASED on a pre-Complete error. The stated reason is that "the
// target must never hold a server-side prepared statement the budget didn't
// admit" — so the budget decides before the frame goes out, never after.
//
// WHAT AN OBJECT'S CHARGE IS (jarvis's ruling, 2026-09-04): the transferred
// SEGMENT charge, which §1.5 defines as the frame's own two-stage figure — its
// declared wire length, plus the decoded delta for what the frame pre-allocates.
// A statement retains what its Parse frame was charged; a portal what its Bind
// frame was charged. There is no separate per-object figure to invent.
//
// And it under-counts what the TARGET holds server-side ON PURPOSE. §1.4 calls
// this the RESIDENT-memory budget: the target's memory is the target's, and the
// 16 MiB cap bounds THIS process. Nobody should later "correct" this upward.

// retainedBudgetPerSession is §9's default retained-state quota (16 MiB/session,
// ceiling 64 MiB). Exceeding it refuses the statement; the connection stays.
const retainedBudgetPerSession int64 = 16 << 20

// §9's per-session namespace limits (matrix :385, :479).
//
// NAMED objects only. The unnamed statement and portal are exempt because they
// are REPLACED rather than accumulated — a client that re-Parses the unnamed
// statement a million times holds one object, and counting those would refuse a
// correct client for doing the one thing the unnamed name is for.
const (
	maxNamedStatements = 256
	maxNamedPortals    = 64

	// maxBindParams is §9's per-Bind parameter limit (matrix :384, :484). Well
	// under the protocol's own int16 ceiling of 65535: the figure that matters
	// here is what one frame may make US pre-allocate, and §1.5 charges the
	// parameter and format arrays as the frame's stage-2 delta.
	maxBindParams = 8192
)

// namedStatements and namedPortals count the capped objects.
//
// DERIVED, NOT TRACKED. A counter incremented in put and decremented in every
// drop is a second bookkeeping with its own drift, and §4a has five release
// points to keep in step; the map is the truth and the unnamed entry is the only
// exemption, so this is O(1) and cannot disagree with the store.
func (o *extObjects) namedStatements() int {
	n := len(o.statements)
	if _, ok := o.statements[""]; ok {
		n--
	}
	return n
}

func (o *extObjects) namedPortals() int {
	n := len(o.portals)
	if _, ok := o.portals[""]; ok {
		n--
	}
	return n
}

// objectCharge is the retained figure for a frame that creates an object: the
// bytes it declares plus what it makes the front door hold for it.
//
// payload is the frame's own variable content (a statement's SQL, a portal's
// parameter values); extra is the decoded delta the frame pre-allocates (a
// Bind's parameter and format arrays). frameOverhead covers the fixed header and
// the name, which are small but not zero — a session that parses ten thousand
// empty statements is still holding ten thousand objects.
func objectCharge(payload, extra int) int64 {
	const frameOverhead = 64
	return int64(payload) + int64(extra) + frameOverhead
}

// reserveRetained admits an object's charge against the session's budget, or
// refuses with ErrRetainedBudget having admitted nothing.
func (o *extObjects) reserveRetained(charge int64) error {
	if o.retained+o.retainedPending+charge > retainedBudgetPerSession {
		return ErrRetainedBudget
	}
	o.retainedPending += charge
	return nil
}

// finalizeRetained moves a pending charge to retained when the target's
// completion arrives.
//
// ONE CRITICAL SECTION, and that is a requirement rather than a convenience:
// §8's "no double-charge, no gap" is a statement about what a concurrent reader
// may observe, so pending down and retained up happen together or a reader sees
// a total that was never true.
//
// A completion for an object the store no longer holds is a NO-OP. It is
// reachable — a second Parse replaces the unnamed statement while the first
// completion is still in flight — and the temptation is to release the charge
// here. That would be a DOUBLE RELEASE: the drop already released it, because
// the §4a release points own every charge an object holds, pending or finalized.
// One owner. The counter below exists so the case is visible rather than silent.
func (o *extObjects) finalizeRetained(ref objectRef) {
	var obj *retainedRef
	switch ref.kind {
	case objectStatement:
		if st, ok := o.statements[ref.name]; ok && st.seq == ref.seq {
			obj = &retainedRef{charge: &st.charge, finalized: &st.finalized}
		}
	case objectPortal:
		if p, ok := o.portals[ref.name]; ok && p.seq == ref.seq {
			obj = &retainedRef{charge: &p.charge, finalized: &p.finalized}
		}
	}
	if obj == nil {
		o.lateCompletions++ // the object is gone; its drop already released it
		return
	}
	if *obj.finalized {
		return // PortalSuspended re-finalizing an already-finalized portal
	}
	*obj.finalized = true
	o.retainedPending -= *obj.charge
	o.retained += *obj.charge
}

// releaseRetained returns an object's charge, whichever phase it was in. Called
// ONLY from the §4a release points, which own it.
func (o *extObjects) releaseRetained(charge int64, finalized bool) {
	if finalized {
		o.retained -= charge
		if o.retained < 0 {
			o.retained = 0
		}
		return
	}
	o.retainedPending -= charge
	if o.retainedPending < 0 {
		o.retainedPending = 0
	}
}

// sweepUnfinalized DESTROYS every object whose completion will never arrive, and
// is the shared implementation behind two callers: Sync, and a Terminate
// mid-segment.
//
// After a target ErrorResponse the segment discards to Sync, so the answers to
// everything queued behind it never come. Those objects were never created ON
// THE TARGET — the Parse that would have created them was discarded — so the
// store holding a record of them means the store and the backend disagree about
// what exists.
//
// IT DROPS THE OBJECT, NOT ONLY ITS CHARGE (jarvis's ruling, 2026-09-03). Two
// reasons, and the second is why this belongs with the caps rather than with the
// accounting:
//
//  1. A record the target does not have is a phantom. A later Bind or Execute
//     naming it must fail as PostgreSQL's own does — 26000 for a statement,
//     34000 for a portal — rather than being relayed to a backend that will
//     answer the same thing less clearly.
//  2. A phantom COUNTS AGAINST THE NAMED-OBJECT CAP. Releasing the charge but
//     keeping the record leaves 256/64 slots slowly consumed by objects that do
//     not exist anywhere, until legitimate Parses are refused. That is not a
//     fidelity nit; it is a slow refusal of correct work.
//
// The drops own the release, here as everywhere else — this walks them rather
// than releasing separately, so there is still exactly one owner per charge.
func (o *extObjects) sweepUnfinalized() {
	for name, p := range o.portals {
		if !p.finalized {
			o.dropPortal(name)
		}
	}
	for name, st := range o.statements {
		if !st.finalized {
			// The cascade takes its portals with it: a statement the target never
			// created cannot have portals bound to it on the target either.
			o.dropStatement(name)
		}
	}
}

// dropObject releases an object by reference, for the drain's pre-Complete error
// path. It routes to the same drops every other release point uses.
func (o *extObjects) dropObject(ref *objectRef) {
	switch ref.kind {
	case objectStatement:
		if st, ok := o.statements[ref.name]; ok && st.seq == ref.seq {
			o.dropStatement(ref.name)
		}
	case objectPortal:
		if p, ok := o.portals[ref.name]; ok && p.seq == ref.seq {
			o.dropPortal(ref.name)
		}
	}
}

// retainedRef is one object's accounting fields, so finalize handles both kinds
// without duplicating the transfer.
type retainedRef struct {
	charge    *int64
	finalized *bool
}
