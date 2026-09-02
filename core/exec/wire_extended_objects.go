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
	} else if _, live := o.statements[""]; live {
		o.dropStatement("")
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
	} else if _, live := o.portals[""]; live {
		o.dropPortal("")
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
	for portalName := range st.portals {
		delete(o.portals, portalName)
	}
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
