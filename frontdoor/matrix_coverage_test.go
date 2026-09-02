package frontdoor

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// F4 protocol conformance — coverage of the matrix, as a gate rather than an
// audit somebody remembers to run.
//
// docs/front-door/protocol-matrix.md is the front door's normative spec, and
// ADR-0075 makes F4 responsible for a "protocol conformance suite". A suite is
// only conformance if its RELATIONSHIP TO THE SPEC is checked: a matrix row
// nobody tests is indistinguishable, from inside the test output, from one
// nobody wrote yet. This turns that question into a failing test.
//
// The mechanism is the citation convention the existing cells already follow —
// comments naming "row 2.1", "matrix row 2.5" — so nothing new is imposed on
// how tests are written.
//
// COVERAGE IS PER-SECTION, and the sections are declared in coveredSections:
// §2's numbered rows, §3.1's parameter table, §4's frontend message matrix,
// §4a's object-release rules, and §5's backend emission matrix, plus the
// prose units in proseUnits (§3.2, §3.3, §4's post-error discard block) —
// normative text that carries no table. A section NOT listed is ungated: the
// coverage boundary is a decision, and it lives here where a reviewer can see
// it, not inside a regex.
//
// Only §2's rows are numbered. Every other covered section keys its rows by
// name — a parameter, a message, an event — so those rows carry
// section-qualified keys derived mechanically from the table's first cell:
// `3.1:client_encoding`, `4:Parse`, `4a:Close-S-name`, `5:ErrorResponse-target`.
// Tests cite them in the same "row <id>" shape §2 already uses — "row
// 4:Parse" — so future cells copy ONE convention, not one per section. The
// derivation is mechanical on purpose: a reworded row changes its key and
// fails the phantom check loudly, exactly as a renumbered §2 row does, rather
// than leaving a hand-maintained alias table to drift silently.
//
// CLAIMS (PR #40 r0 MF1): a matrix row that contains SEPARATELY testable
// guarantees is tracked per claim in claimTriage, keyed "<row>#<claim>" and
// cited as "row 3.1:options#empty-audit". A covered claim's evidence is
// machine-checked by anchors — literal fragments of the proving cell — so
// deleting the case reddens the gate even if the citation comment survives.
// A parent row is DERIVED from its claims (all covered ⇒ covered, else
// awaiting) and the gate checks the hand-written parent state agrees.
// The generated key set is GLOBALLY unique (r0 MF2), and prose units are
// owned by their section — the anchor proves the block stayed where the
// registry says it lives (r0 MF3).

// rowState is the triage every matrix row must carry. A row in neither state
// fails the gate: that is a row somebody added to the spec without deciding
// whether it is testable yet.
type rowState int

const (
	// covered: at least one test cites this row today. The gate fails if the
	// citation disappears, which is how a deleted cell gets noticed.
	covered rowState = iota
	// awaiting: no cell exists yet, with the reason recorded. The gate fails
	// if a citation APPEARS — that is the promotion signal, and it keeps this
	// list from silently rotting into a list of lies.
	awaiting
	// uncited: a cell DOES exist and is named, but it does not cite the row,
	// so the citation scan cannot see it.
	//
	// This state exists because the first version of this gate did not have
	// it, and was wrong because of that: I marked 2.1b and 2.4 "awaiting" and
	// reported 6 of 12 rows untested, when both are in fact tested — 2.4 end
	// to end, over the wire, asserting the uniform denial AND the audit
	// reason. Tested-but-uncited is indistinguishable from untested to a
	// citation scan, so without a third state the gate silently overstates the
	// gap and its headline number is wrong.
	//
	// The named test must EXIST, and the gate checks that it does — otherwise
	// this state would be a free-text excuse for anything.
	uncited
)

// The triage. Every row of every covered section, and why.
//
// Kept in code rather than in the doc on purpose: a claim about what is tested
// belongs where it can be checked against the tests, not beside the prose it
// describes.
//
// `awaiting` reasons name the phase that owes the cell. Most §4/§4a/§5 rows
// are awaiting F1/F2 — post-auth behaviour that does not exist yet. That is
// the correct answer, not a failure of the gate: the matrix describes the
// finished front door, and the gate's job is to keep the distance between
// here and there visible, not to paper over it.
var matrixTriage = map[string]struct {
	state  rowState
	reason string // for `uncited`, this MUST be the test function's name
}{
	// ---- §2 Startup & authentication sequence ----
	"2.1":  {covered, "TestStartup_PlaintextIsRefused"},
	"2.1a": {covered, "direct-TLS ClientHello refusal"},
	"2.1b": {covered, "TestLoadServerTLS_RefusesUnusableMaterial + admission_test's handshake-grinding cell"},
	"2.2":  {covered, "TestStartup_GSSEncIsRefusedWithN"},
	"2.3":  {covered, "TestCancel_TheWireKeyIsTheRegisteredKey + TestCancel_AppliedPairIsAuditedAndClosed + TestCancel_StalePairIsASilentNoOp + TestCancel_TheKeyDiesWithItsSession — the listener half: issuance at open, §6.4 processing of the presented pair, applied/stale audit, revocation at close"},
	"2.4":  {covered, "TestStartup_RefusedParameterIsAuditedButNotDisclosed"},
	"2.5":  {covered, "TestStartup_VersionNegotiation"},
	"2.5a": {covered, "TestStartup_VersionNegotiation, unsupported major"},
	"2.6":  {covered, "AuthenticationCleartextPassword offer — F0e's credential exchange"},
	"2.7":  {covered, "F0d verification chain + atomic reservation"},
	"2.8":  {covered, "type-p frames that are not a PasswordMessage — F0e"},
	"2.9":  {covered, "AuthenticationOk / ParameterStatus / BackendKeyData / ReadyForQuery — F0e"},

	// ---- §3.1 Accepted StartupMessage parameters ----
	// Rows whose guarantees are SEPARATELY testable are marked partial by
	// carrying claims in claimTriage below. The rule is mechanical: a row
	// with claims is covered only when every claim is covered, otherwise it
	// is awaiting — and the gate checks the map agrees with that derivation
	// (a parent whose state contradicts its claims is a failure direction
	// of its own). Claims may be proved in other packages; the witness binding
	// below keeps those citations and anchors inside the named test cells.
	"3.1:user":                {covered, "claims below prove requiredness and the PAT-owner cross-check"},
	"3.1:database":            {covered, "claims below prove requiredness and the grant-on-target check"},
	"3.1:application_name":    {covered, "derived from its claims — #accepted, #truncate-notice-256 and #session-audit are all covered as of #58's seam and its consumer wiring. The parent was held awaiting by #session-audit alone (lector PR #46 r0 MF1); that claim is now witnessed on both sides of the seam, so the derivation promotes it rather than any hand edit here"},
	"3.1:client_encoding":     {awaiting, "partial — claims below (the UTF8-only gate is proven; the target-lease UTF8 pin, ruling 2, needs the F1 WIRE LOOP — WireExecute (#41) has no wire caller; defaultSession is still the F0e 0A000 stub)"},
	"3.1:options":             {covered, "derived from its claims — GUC refusal and empty-accepted (TestStartup_ParameterPolicy), empty-options audit (TestStartup_EmptyOptionsIsAuditedAsIgnored)"},
	"3.1:replication":         {covered, "single claim below — refused, every tested value"},
	"3.1:_pq_":                {covered, "TestStartup_UnrecognizedProtocolOptionsAreNamed + TestStartup_ParameterPolicy — negotiated, not refused"},
	"3.1:any-other-parameter": {covered, "TestStartup_ParameterPolicy — an unknown parameter is refused as a GUC attempt (over the wire: the 2.4 cell)"},

	// ---- §3.2 / §3.3 prose units ----
	"3.2": {awaiting, "post-auth SET policy is the ADR-0074 gate matrix; needs the F1 WIRE LOOP — WireExecute (#41) has no wire caller; defaultSession is still the F0e 0A000 stub; the startup half is 3.1:options' refusal"},
	"3.3": {covered, "TestPGLoop_TheClientReceivesTheTargetsOwnParameterSet + TestPGLoop_TheOverridesWinOverTheTargetsOwnValues — the forwarded half exists as of #58's seam and is proven against an INDEPENDENT connection to the same target (SHOW server_version/server_encoding/client_encoding/DateStyle), not against a list written in the cell, which would have encoded this build's parameters and passed while the relay forwarded something else. The overrides are proven to WIN over the target's own values for the same names, which is why they are appended after the forwarded set rather than the set being filtered; mutation-proven both ways (drop the forwarding, and reverse the order)"},

	// ---- §4 Frontend message matrix (post-auth) ----
	"4:Query":                     {covered, "TestPGLoop_MultiStatementRunsInOrderAndStopsAtTheFirstError + TestPGLoop_ControlInsideTheBufferDrivesTheTransactionState — implicit-block semantics against a real server: statements run in order, the first error rolls the block back (count(*)=0 afterwards), and BEGIN/COMMIT inside one buffer drive the readiness byte"},
	"4:Parse":                     {covered, "F2 routes and GATES it (classifier + profile + grants + the rule-2 reader stage, live-celled) and F2b reserves the statement's retained capacity BEFORE the Parse is forwarded, finalizing at ParseComplete and releasing on error - TestRetained_BudgetRefusalAdmitsNothing proves a refusal admits nothing, and the mutation making the budget never refuse reddens it"},
	"4:Bind":                      {covered, "F2 routes it and relays raw parameter formats and OIDs verbatim (live-celled, binary in both directions); F2b reserves the portal's retained capacity before forwarding, and F2b-C refuses a Bind past the 8192-parameter limit before the frame is forwarded - TestCaps_ABindPastTheParameterCapIsRefusedAndAdmitsNothing admits the whole allowance, refuses one more, and proves the refusal creates no portal and moves no charge"},
	"4:Describe":                  {covered, "TestPGExtended_DescribeCarriesTheServersOwnOIDs + TestPGExtended_AStatementWithNoResultColumnsDescribesAsNoData - a live client drives Describe('S') and Describe('P') end to end. The discriminator is three DIFFERENT result types (int4/bool/text): a re-deriving producer collapses them to text OID 25, so equal-to-text would pass a weaker cell. NoData is asserted as its own frame rather than an empty RowDescription"},
	"4:Execute":                   {awaiting, "awaiting - and NARROWER than before, not closed. TestPGExtended_ARowLimitedFetchSuspendsAndResumes closes the maxRows/PortalSuspended half the previous reason named: N rows then PortalSuspended REACHES THE CLIENT, and the resumption returns the remainder and ends in CommandComplete. TWO contracted halves remain unwitnessed and the row must not be promoted on the first: (1) the row says authority is re-resolved at EVERY Execute, PORTAL RE-EXECUTIONS INCLUDED - the existing live proof revokes a grant between Parse and Execute, which is not a re-execution, so a resumption riding the first Execute's authority would pass everything written today (white-vision: the discriminating cell revokes BETWEEN the first Execute and the resumption); (2) fd.stmt_attempt/fd.stmt_outcome PER Execute - three paged Executes are three rows, not one covering the portal, and a producer folding them looks perfect on the wire. Neither is witnessed here, and the reason for (2) is that it is the WRONG HOME rather than out of reach: attempt rows are engine-side audit, not listener events, so a frontdoor cell would have to open its own meta-store handle against the same database - pgLoopWithEngine builds one and does not return it. Awkward, not impossible; white-vision corrected an earlier version of this reason that said 'not observable at all', which was too strong and would have outlived the message that contained it. I promoted this row on the maxRows half alone and corrected it before review rather than after"},
	"4:Close":                     {covered, "F2 routes both object types, the 4a cascade is unit-celled, and F2b makes the drop the OWNER of the release - TestRetained_EveryReleasePointReturnsItsCharge walks Close-S cascade, Close-P, transaction end and simple-Query destruction, and the mutation letting a drop skip a still-pending charge reddens it"},
	"4:Flush":                     {awaiting, "awaiting - and now WITNESSED ABSENT rather than merely undriven. TestPGExtended_AStandaloneFlushDeliversNothing drives a client Flush and receives NOTHING: no frames on the wire until the client Syncs. The cell observes the WIRE only - it does not read the meta store, so this reason claims wire silence and not an absence of audit rows. The session RECOVERS through a subsequent Sync, so this is a missing capability rather than a broken session - a severity I first recorded as worse, from a racy version of the cell whose goroutine corrupted the stream. The row stays AWAITING because that cell pins the defect, it does not witness the contract; promoting it would make this list say the behaviour works. Reported to jarvis and white-vision; the real witness is written in that cell's failure message, to be restored when Flush dispatches"},
	"4:Sync":                      {covered, "segment close, the engine's own ReadyForQuery and the lane-reservation release are live-celled; F2b adds the segment-counter reset (10000 msgs / 96 MiB) and the sweep of every reservation the aborted segment will never confirm - TestExtCaps_SyncResetsTheSegmentCounters drives two under-cap segments whose sum is over it, and TestExtPG_SyncSweepsWhatTheAbortedSegmentWillNeverConfirm drives a real Sync rather than the sweep helper"},
	"4:Terminate":                 {covered, "derived from its three claims — clean-close, release and rollback are each proven; the rollback claim by TestPGLoop_TerminateRollsBackAnOpenTransaction across two connections"},
	"4:CopyData":                  {covered, "TestLoop_CopyDataIsFatalAndCloses — CopyData/CopyDone/CopyFail are a fatal 08P01 and the connection closes; audited under its own cause"},
	"4:FunctionCall":              {covered, "TestLoop_FunctionCallIsRefusedAndTheConnectionStillWorks + TestLoop_SurvivingRefusalEndsTheCycleWithReadiness — a REFUSAL, not a violation: 0A000 frontdoor/no-fastpath, the cycle ends with ReadyForQuery, and the same connection then answers a query"},
	"4:Unknown-message-type-byte": {covered, "TestLoop_UnknownMessageTypeIsFatalAndNotSkipped — an undefined type byte is a fatal 08P01 and the stream closes; the cell sends a valid Query immediately after and requires it is never answered, which is what proves not-skipped-and-continued"},
	"4:discard":                   {covered, "TestPGExtended_AMidSegmentErrorDiscardsThroughSync - after a mid-segment ErrorResponse every further frame is discarded (a Parse produces no second ParseComplete, a Query produces no rows), Sync ends the discard, and the session is usable afterwards. Uses a VOLATILE divisor so the error is raised at Execute: SELECT 1/0 folds at plan time and would have driven a Bind-time error instead, which the cell asserts against by requiring BindComplete"},

	// ---- §4a Object-release rules ----
	"4a:Close-S-name":      {covered, "the Close-S cascade destroys the statement and its portals (unit-celled, mutation-proven) AND releases their retained charges - TestRetained_EveryReleasePointReturnsItsCharge/Close-S_cascade, with the drop as the single owner of the charge; the mutation that lets a drop skip a still-pending charge reddens it"},
	"4a:Close-P-name":      {covered, "the portal dies and its charge goes back - TestRetained_EveryReleasePointReturnsItsCharge/Close-P, same owner rule and same mutation"},
	"4a:Parse":             {covered, "unnamed-statement replacement destroys the old object and RELEASES its charge - TestRetained_ReplacingAnUnnamedObjectReleasesTheOldCharge asserts the account holds only the replacement's charge afterwards, and TestRetained_ARefusedUnnamedReplacementLeavesTheOldStatementUsable proves a refused replacement destroys nothing"},
	"4a:Bind":              {covered, "unnamed-portal replacement, same two halves and the same two cells"},
	"4a:Query":             {covered, "a simple Query on a wire session destroys the unnamed pair (proven as a unit AND live) and releases both charges - TestRetained_EveryReleasePointReturnsItsCharge/simple_Query_destroys_the_unnamed_pair"},
	"4a:Transaction-end":   {covered, "all portals die at transaction end, hooked at clearTxLocked (the single point every transaction end passes), and their charges are released there - TestRetained_EveryReleasePointReturnsItsCharge/transaction_end_then_Close-S"},
	"4a:Error-mid-segment": {covered, "an in-flight reservation is released when the target refuses the frame that would have created the object - TestRetained_PreCompleteErrorReturnsTheCharge, and live in TestExtPG_SyncSweepsWhatTheAbortedSegmentWillNeverConfirm where everything queued behind the error is both released and destroyed"},
	"4a:Session-end":       {awaiting, "awaiting - the segment lane reservation is released on every exit including teardown (celled via general.inUse()), and retained charges are now accounted, but NO cell asserts what teardown leaves behind: the per-session account dies with the session object, so there is nothing to release, and that is an argument rather than a witness. A cell would have to observe the lane and the session registry after a mid-segment teardown"},

	// ---- §5 Backend emission matrix ----
	"5:RowDescription":                  {covered, "TestPGLoop_RowDescriptionCarriesTheServersTypes — the OIDs are the SERVER's (23/25/16), the values are its own rendering and the command tag is verbatim; a decode-and-re-encode producer would report text OID 25"},
	"5:ErrorResponse-target":            {covered, "TestPGLoop_TargetErrorIsVerbatimIncludingPosition — Position, File and Routine all survive (no front door can compute Position) and the DETAIL is asserted NOT to be a front-door rule id, which is what separates a forwarded error from a synthesized one"},
	"5:ErrorResponse-gate-front-door":   {covered, "TestLoop_GateRefusalIsFramedWithTheGateIdentityThenReadiness — §8a identity with the rule id in DETAIL, never the uniform pre-auth code, and the readiness that follows carries the ENGINE's state so a refusal inside a transaction does not read as idle"},
	"5:ReadyForQuery":                   {covered, "TestLoop_ReadyForQueryCarriesTheEnginesStatus + TestLoop_InvalidStatusByteIsNeverForwarded — synthesized from the ExecSession status across all three bytes, and a status outside the protocol's three is refused rather than put on the wire"},
	"5:AuthenticationCleartextPassword": {covered, "TestAuth_OffersCleartextAndNothingElse + TestAuth_SuccessSequence + TestStartup_VersionNegotiation — prompt, success group, and protocol negotiation"},
	"5:CopyInResponse":                  {covered, "TestLoop_ImpossibleBackendMessageIsAFrontDoorDefectAndCloses — a never-emitted backend message arriving from the target is a front-door defect: fatal, closed, and audited under its own cause rather than skipped"},
}

// claimTriage is the claim-level obligation registry (lector PR #40 r0 MF1):
// a matrix row that contains SEPARATELY testable guarantees is tracked per
// claim, keyed "<row>#<claim>" and cited in tests as "row 3.1:options#empty-audit".
//
// A covered claim carries a WITNESS — the test function that proves it —
// and ANCHORS — literal fragments of the proving cell. The evidence is
// machine-checked with CELL ownership (r2 MF1): the citation AND every
// anchor must appear inside the witness function's span, located by Go AST
// positions (doc comment through closing brace), not by regex function
// guessing. Deleting the proving case reddens the gate even if the citation
// comment survives (r0, mutation M12); planting the anchor in an unrelated
// comment — same file or not — is not evidence (r1/r2, mutations M19/M21).
// An awaiting claim is a missing obligation with the phase that owes it;
// claims carry exactly two states, and the registry refuses anything else
// (r2 MF2), including blank anchors, which match everything and therefore
// prove nothing (r2 MF3).
//
// Parent rows are DERIVED from their claims: all-covered ⇒ the parent is
// covered, anything awaiting ⇒ the parent is awaiting. matrixTriage must
// agree with the derivation — the gate checks it (mutation direction: a
// parent whose hand-written state contradicts its claims reddens).
var claimTriage = map[string]struct {
	state   rowState // exactly covered or awaiting; anything else is a registry fatal
	witness string   // the test FUNCTION that proves a covered claim
	reason  string
	anchors []string // required for covered claims: literal fragments of the proving cell; all must match inside the witness
}{
	"3.1:user#required": {covered, "TestStartup_RequiredParameters",
		"the required pair, without the check a blank startup reads as an auth problem",
		[]string{`"database": "lm-prod"}, "user"}`}},
	"3.1:user#owner-cross-check": {covered, "TestOpenWireSession_EveryRefusalIsAuditedDistinctlyAndDeniedUniformly",
		"the startup user must match the PAT owner",
		[]string{"the startup user is not the token's owner", "DenyUserMismatch"}},

	"3.1:database#required": {covered, "TestStartup_RequiredParameters",
		"the required pair",
		[]string{`"user": "root"}, "database"}`}},
	"3.1:database#grant-on-target": {covered, "TestOpenWireSession_TheRemainingRefusals",
		"the authenticated user must have a grant on the presented target",
		[]string{"a user with no grant on the target", "DenyNoGrant"}},

	"3.1:application_name#accept": {covered, "TestStartup_ParameterPolicy",
		"the pinned set accepts application_name",
		[]string{`"application_name": "psql"`}},
	"3.1:application_name#truncate-notice-256": {covered, "TestStartup_ApplicationNameIsCappedAt256Bytes",
		"over 256 bytes: the echoed ParameterStatus is the rune-safe 256-byte prefix, a NoticeResponse is sent, and fd.param_truncated audits the verbatim original",
		[]string{"fd.param_truncated", "*pgproto3.NoticeResponse", "applicationNameMaxBytes"}},
	// The THIRD guarantee in row 199 — "recorded on session + every audit row" — had
	// no claim, so the derivation could not see it and the parent was falsely
	// promoted (lector PR #46 r0 MF1). A missing claim is invisible to the gate;
	// only a present-and-awaiting one keeps the parent honest.
	"3.1:application_name#session-audit": {covered, "TestLoop_TheAcceptedApplicationNameReachesTheEngine",
		"the front door's half — the ACCEPTED label reaches the ENGINE, not merely the echo. The distinction is the claim: an echo synthesized here from the startup params would satisfy a wire-only cell while the session recorded nothing and every audit row said app \"\". The engine's half (recorded on the session and on every audit line) is core/exec's TestWireOpen_ApplicationNameIsOnTheSessionAndEveryAuditLine and TestWireOpen_AuditStampCoversDecodedAndOwnedControlSites, shipped with #58",
		[]string{"openedAppNames", "reporting-tool-7"}},

	"3.1:client_encoding#utf8-only": {covered, "TestStartup_ParameterPolicy",
		"non-UTF8 refused, hyphen spelling tolerated",
		[]string{`"client_encoding": "LATIN1"`, `"client_encoding": "utf-8"`}},
	"3.1:client_encoding#lease-utf8-pin": {awaiting, "",
		"the target lease is pinned UTF8 at acquisition (ruling 2) — nothing in core/exec pins client_encoding yet; needs the F1 WIRE LOOP — WireExecute (#41) has no wire caller; defaultSession is still the F0e 0A000 stub", nil},

	"3.1:options#guc-refusal": {covered, "TestStartup_ParameterPolicy",
		"both spellings refused",
		[]string{`"options": "-c search_path=public"`, `"options": "--search_path=public"`}},
	"3.1:options#empty-accepted": {covered, "TestStartup_ParameterPolicy",
		"empty/whitespace accepted and ignored",
		[]string{`"options": "   "`}},
	"3.1:options#empty-audit": {covered, "TestStartup_EmptyOptionsIsAuditedAsIgnored",
		"an empty/whitespace options is accepted, ignored, and audited as fd.param_ignored; a startup without options emits no such event",
		[]string{"fd.param_ignored", "no options key at all"}},

	// ---- 4:Terminate — three separately testable guarantees (lector PR #45 r0 MF1).
	// The original single-row promotion cited two cells that observe reservation
	// release on ANY teardown: they send Terminate, close the socket themselves, and
	// never Receive — so a server that emitted 0A000 before closing stayed green
	// (Juliet's behaviour-removal mutation). Clean close needs its own witness that
	// READS the wire after Terminate and requires the connection to end with no frame.
	"4:Terminate#clean-close": {covered, "TestSession_TerminateClosesTheWireWithoutAnErrorFrame",
		"after Terminate the server closes the connection and sends NOTHING — no ErrorResponse, no ReadyForQuery",
		[]string{"want the connection closed with NO frame", "ne.Timeout()", "left the session OPEN"}},
	"4:Terminate#release": {covered, "TestSession_TerminateReleasesTheReservation",
		"the session's reservation is released when the client leaves — proven as release-on-teardown, which Terminate is one cause of",
		[]string{`waitFor(t, "the release"`, "len(closed) == 1"}},
	"4:Terminate#rollback": {covered, "TestPGLoop_TerminateRollsBackAnOpenTransaction",
		"an open transaction is rolled back on Terminate — proven across connections: the write is visible inside its own transaction, Terminate arrives with no COMMIT, and a SECOND connection sees nothing",
		[]string{"BEGIN", "Terminate", "SELECT count(*) FROM"}},

	"3.1:replication#refused-any-value": {covered, "TestStartup_ParameterPolicy",
		"refused at every tested value",
		[]string{`"replication": "database"`}},
}

var (
	// A §2 row opens a table line: "| 2.1a | ..." — matched line-by-line
	// inside §2's section body only (see matrixRowIDs), so a numbered row in
	// some OTHER section is outside the declared coverage boundary rather
	// than a surprise triage demand.
	matrixRowRe = regexp.MustCompile(`^\|\s*([0-9]+\.[0-9]+[a-z]?)\s*\|`)
	// A citation in a test, in the shapes the existing cells already use.
	//
	// CASE-INSENSITIVE, and that is not cosmetic. The first version was not,
	// and PR #36 writes its citations as "// MATRIX ROW 2.4" — a good-faith
	// citation the gate could not see, so both rows would have read as
	// untested while the comment sat directly above the cell proving them.
	// Found by zen. A comment is prose, and a gate that silently ignores
	// prose it does not like is a gate reporting a gap that is not there.
	//
	// QUALIFIED FIRST, and the order is load-bearing: "row 3.1:user" must
	// register a citation for 3.1:user, not silently for a bare "3.1". The
	// bare numeric form — §2's ids, and the §3.2/§3.3 prose-unit ids — is
	// the FALLBACK, tried only when no ":"-qualified key follows the id.
	// A bare-first ordering would record "row 3.1:user" as a citation of a
	// row 3.1 that may exist independently, and nothing anywhere would fail.
	// The section number may carry a LETTER SUFFIX, because §4a is a section and
	// its rows are written "4a:Parse". Without it no §4a row could ever be cited:
	// the pattern required a digit or a dot after the number, so every citation
	// of a §4a row was invisible and all eight sat permanently awaiting with no
	// way to promote them (found while reconciling them, F2b commit B r1).
	citationRe = regexp.MustCompile(`(?i)\browz?\s+((?:[0-9]+[a-z]?(?:\.[0-9]+[a-z]?)?):[A-Za-z0-9_-]+(?:#[A-Za-z0-9_-]+)?|[0-9]+\.[0-9]+[a-z]?)`)

	// A markdown heading: "## 2. Startup...", "### 4a. Object-release...".
	headingRe = regexp.MustCompile(`^(#{2,4})\s+(.+)$`)
	// A backticked span in a table's first cell: "`Parse` naming...".
	backtickSpanRe = regexp.MustCompile("`([^`]+)`")
	// A parenthetical immediately after a row identifier: "`ErrorResponse` (target)".
	adjacentParenRe = regexp.MustCompile(`^\s*\(([^)]*)\)`)
	// A markdown table separator line: "|---|---|...".
	separatorRe = regexp.MustCompile(`^\|(\s*:?-{3,}:?\s*\|)+$`)
)

// coveredSections declares the sections this gate claims to cover. A section
// not listed here is ungated — deliberately; see the file comment.
type matrixSection struct {
	id      string // the heading id as written in the doc: "2", "3.1", "4", "4a", "5"
	numeric bool   // §2's rows are numbers in the first cell; every other section keys by name
}

var coveredSections = []matrixSection{
	{"2", true},
	{"3.1", false},
	{"4", false},
	{"4a", false},
	{"5", false},
}

// proseUnits are normative blocks that carry no table: §3.2's GUC policy and
// §3.3's ParameterStatus set are prose subsections, and §4's post-error
// discard rule is a prose block inside §4. They are triaged like rows — same
// map, same three states — and their existence is asserted by ANCHOR so a
// renamed or reworded heading fails loudly instead of the unit silently
// dropping out of the triage forever.
//
// OWNERSHIP IS SECTION-BOUND (r0 MF3): each unit's anchor is matched
// against its OWNING section's span — heading to the next heading of the
// same-or-higher level — never against the whole document. A phrase that
// drifted into another section is a DIFFERENT unit; the gate's job is to
// prove the block stayed where the registry says it lives, not that the
// words exist somewhere (r0 relocated the discard block past §5 and the
// gate stayed green; that direction is bound by mutation M17).
var proseUnits = []struct {
	key     string
	section string // the owning section: the anchor matches inside this span
	anchor  *regexp.Regexp
}{
	{"3.2", "3", regexp.MustCompile(`(?m)^###\s+3\.2\b`)},
	{"3.3", "3", regexp.MustCompile(`(?m)^###\s+3\.3\b`)},
	{"4:discard", "4", regexp.MustCompile(`Post-error segment discard`)},
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// This package sits one level below the module root.
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

// headingBody returns the body of the section whose heading id is id — the
// first token of the heading text after the #s ("2", "3.1", "4a"): everything
// from that heading line up to the NEXT heading of any level. For "4" that
// stops at "### 4a.", giving §4's own table and its discard prose but not
// §4a's table; for "3.1" it stops at "### 3.2".
func headingBody(t *testing.T, src, id string) string {
	t.Helper()
	want := regexp.MustCompile(`^` + regexp.QuoteMeta(id) + `([.\s]|$)`)
	var body []string
	inBody := false
	for _, ln := range strings.Split(src, "\n") {
		if m := headingRe.FindStringSubmatch(ln); m != nil {
			if inBody {
				break // the next heading, whatever its level, ends the section
			}
			if want.MatchString(strings.TrimSpace(m[2])) {
				inBody = true
				continue
			}
		}
		if inBody {
			body = append(body, ln)
		}
	}
	if !inBody {
		t.Fatalf("the matrix has no §%s heading — the section was renumbered, renamed, or removed, and this gate refuses to run blind against a spec whose shape it cannot find", id)
	}
	return strings.Join(body, "\n")
}

// headingSpan returns the FULL span of the section whose heading id is id:
// from that heading line up to the next heading of the SAME-OR-HIGHER level.
// Unlike headingBody (which stops at the next heading of any level, to keep
// §4's table out of §4a's), the span of §4 includes its ### subsections —
// which is what a prose unit's ownership check needs: the block stays
// anywhere inside its owning section, and nowhere else.
func headingSpan(t *testing.T, src, id string) string {
	t.Helper()
	want := regexp.MustCompile(`^` + regexp.QuoteMeta(id) + `([.\s]|$)`)
	var body []string
	inBody := false
	myLevel := 0
	for _, ln := range strings.Split(src, "\n") {
		if m := headingRe.FindStringSubmatch(ln); m != nil {
			level := len(m[1])
			if inBody && level <= myLevel {
				break
			}
			if !inBody && want.MatchString(strings.TrimSpace(m[2])) {
				inBody = true
				myLevel = level
				continue
			}
		}
		if inBody {
			body = append(body, ln)
		}
	}
	if !inBody {
		t.Fatalf("the matrix has no §%s heading — the section was renumbered, renamed, or removed, and this gate refuses to run blind against a spec whose shape it cannot find", id)
	}
	return strings.Join(body, "\n")
}

// tableDataFirstCells returns the first cell of every DATA row of every
// markdown table in body. Header and separator lines are not data: a data row
// is a table line that follows a separator line, until a non-table line ends
// the table. A data row with an EMPTY first cell is fatal — a row the gate
// cannot key is a row it would silently not track.
func tableDataFirstCells(t *testing.T, body string) []string {
	t.Helper()
	var cells []string
	inData := false
	for _, ln := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(ln)
		if !strings.HasPrefix(trimmed, "|") {
			inData = false
			continue
		}
		if separatorRe.MatchString(trimmed) {
			inData = true
			continue
		}
		if !inData {
			continue // header line, or prose that happens to start with "|"
		}
		c := firstCell(trimmed)
		if c == "" {
			t.Fatalf("a data row with an empty first cell in a covered table — the gate cannot key it and refuses to skip it silently: %q", ln)
		}
		cells = append(cells, c)
	}
	return cells
}

func firstCell(tableLine string) string {
	rest := strings.TrimPrefix(tableLine, "|")
	if i := strings.Index(rest, "|"); i >= 0 {
		rest = rest[:i]
	}
	return strings.TrimSpace(rest)
}

// rowIdent extracts a row's identifier from its first table cell: the first
// backticked span when the cell carries one ("`Parse` naming…" → "Parse"),
// otherwise the cell text up to any parenthetical ("Session end (any cause)"
// → "Session end"). The parenthetical immediately after the identifier is
// returned alongside: it is the disambiguator when two rows of one section
// would derive the same key — §5's two `ErrorResponse` rows.
func rowIdent(cell string) (ident, paren string) {
	if loc := backtickSpanRe.FindStringSubmatchIndex(cell); loc != nil {
		ident = cell[loc[2]:loc[3]]
		if m := adjacentParenRe.FindStringSubmatch(cell[loc[1]:]); m != nil {
			paren = m[1]
		}
		return ident, paren
	}
	ident = cell
	if i := strings.Index(cell, "("); i >= 0 {
		ident = strings.TrimSpace(cell[:i])
		if m := adjacentParenRe.FindStringSubmatch(cell[i:]); m != nil {
			paren = m[1]
		}
	}
	return ident, paren
}

// slugKey normalizes an identifier into a key segment. Case is PRESERVED —
// message names are canonical CamelCase (`4:Parse`, not `4:parse`) — runs of
// whitespace and "/" become a single "-", and characters outside
// [A-Za-z0-9_-] drop ("_pq_.*" → "_pq_", "**Transaction end**" →
// "Transaction-end").
func slugKey(s string) string {
	var b strings.Builder
	pendingDash := false
	for _, r := range s {
		switch {
		case r == ' ' || r == '\t' || r == '/':
			pendingDash = true
		case r == '_' || r == '-' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			if pendingDash && b.Len() > 0 {
				b.WriteByte('-')
			}
			pendingDash = false
			b.WriteRune(r)
		}
	}
	return b.String()
}

// deriveSectionKeys turns first cells into section-prefixed triage keys.
// Two rows deriving the same base key is FATAL unless their adjacent
// parentheticals disambiguate them: a key that addresses two rows is an
// address that addresses neither, and the gate refuses to pretend otherwise.
func deriveSectionKeys(t *testing.T, section string, cells []string) []string {
	t.Helper()
	type entry struct {
		base     string
		disambig string
		cell     string
		hasParen bool
	}
	entries := make([]entry, len(cells))
	for i, c := range cells {
		ident, paren := rowIdent(c)
		entries[i] = entry{
			base:     slugKey(ident),
			cell:     c,
			hasParen: paren != "",
		}
		if entries[i].base == "" {
			t.Fatalf("§%s: the row %q derives an EMPTY key — an identifier with no keyable characters is a row the gate cannot address and refuses to skip", section, c)
		}
		if paren != "" {
			entries[i].disambig = slugKey(ident + " " + paren)
		} else {
			entries[i].disambig = entries[i].base
		}
	}
	byBase := map[string][]int{}
	for i, e := range entries {
		byBase[e.base] = append(byBase[e.base], i)
	}
	keys := make([]string, len(entries))
	for base, idxs := range byBase {
		if len(idxs) == 1 {
			keys[idxs[0]] = section + ":" + base
			continue
		}
		// A collision: every member of the group takes its parenthetical.
		// Disambiguating only one side would leave the bare key assigned by
		// list order — unstable, and wrong in a way no test would catch.
		members := make([]string, len(idxs))
		for j, i := range idxs {
			members[j] = entries[i].cell
		}
		for _, i := range idxs {
			if !entries[i].hasParen {
				t.Fatalf("§%s: the rows %s all key to %q with no parenthetical to tell them apart — the gate cannot address rows it cannot uniquely name", section, strings.Join(members, " | "), section+":"+base)
			}
		}
		for _, i := range idxs {
			keys[i] = section + ":" + entries[i].disambig
		}
	}
	// The parentheticals could themselves collide (two "ErrorResponse
	// (target)" rows); that is still one key for two rows.
	seen := map[string]string{}
	for i, k := range keys {
		if prev, ok := seen[k]; ok {
			t.Fatalf("§%s: rows %q and %q both key to %q even after parenthetical disambiguation — the gate refuses to track two rows under one key", section, prev, entries[i].cell, k)
		}
		seen[k] = entries[i].cell
	}
	return keys
}

// matrixRowIDs reads the matrix and returns every row key the gate claims to
// cover: one parser per covered section, plus the prose units, plus the
// claim keys of claimTriage (validated against their parent rows). Every
// parser carries the contract the §2 one established on day one: a section
// that yields ZERO rows is a FATAL, never a quiet pass — a regex that
// silently matches nothing turns the whole gate into a test that measures
// nothing while reporting success, which is the exact failure this gate
// exists to prevent elsewhere.
//
// The generated key set is GLOBALLY UNIQUE (r0 MF2): per-table collision
// checks cannot see across sections or against prose keys, so uniqueness is
// enforced once over the complete set — a §4 table row named `discard` and
// the prose unit 4:discard are the same finding, not two.
func matrixRowIDs(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "front-door", "protocol-matrix.md"))
	if err != nil {
		t.Fatalf("read the matrix: %v — the conformance gate cannot run without the spec it gates against", err)
	}
	var out []string
	origin := map[string]string{} // key → where it came from, for the uniqueness fatal
	add := func(key, from string) {
		if prev, ok := origin[key]; ok {
			t.Fatalf("the row key %q is generated twice — by %s and by %s. A key that addresses two units addresses neither, and the gate refuses to track it", key, prev, from)
		}
		origin[key] = from
		out = append(out, key)
	}
	for _, s := range coveredSections {
		body := headingBody(t, string(src), s.id)
		if s.numeric {
			for _, k := range numericRows(t, s.id, body) {
				add(k, "§"+s.id)
			}
			continue
		}
		cells := tableDataFirstCells(t, body)
		if len(cells) == 0 {
			t.Fatalf("§%s yielded ZERO table rows: its table moved, changed shape, or left the section — the gate is now measuring nothing in a section it claims to cover", s.id)
		}
		for _, k := range deriveSectionKeys(t, s.id, cells) {
			add(k, "§"+s.id)
		}
	}
	for _, u := range proseUnits {
		span := headingSpan(t, string(src), u.section)
		n := len(u.anchor.FindAllStringIndex(span, -1))
		switch {
		case n == 0:
			t.Fatalf("the prose unit %s is gone from §%s: its anchor no longer matches inside its owning section — a renamed, reworded, or RELOCATED block must be re-anchored deliberately, not left to silently drop out of the triage", u.key, u.section)
		case n > 1:
			t.Fatalf("the prose unit %s's anchor matches %d places inside §%s — an anchor that cannot name ONE block cannot gate it", u.key, n, u.section)
		}
		add(u.key, "prose unit in §"+u.section)
	}
	sort.Strings(out)
	return out
}

// matrixClaimIDs validates the claim registry against the row set and
// returns the claim keys. A claim whose parent row does not exist describes
// coverage of nothing; a claim in a state the claim machinery does not
// implement is refused (r1 MF2, r2 MF2): `uncited` and unknown states skip
// their checks while still deriving coverage — a registry entry that
// behaves differently from what it says is worse than no entry. A covered
// claim must name a witness and carry at least one anchor, and every
// anchor must be non-blank after trimming (r2 MF3): a blank anchor matches
// everything and therefore proves nothing.
func matrixClaimIDs(t *testing.T, rowSet map[string]bool) []string {
	t.Helper()
	var claims []string
	for c, e := range claimTriage {
		parent, _, _ := strings.Cut(c, "#")
		if !rowSet[parent] {
			t.Fatalf("claim %s names the parent row %q, which the matrix does not contain — a claim whose parent is gone describes nothing", c, parent)
		}
		if e.state != covered && e.state != awaiting {
			t.Fatalf("claim %s carries state %d — claims have exactly two states, covered and awaiting; anything else skips every check while still deriving coverage (r2 MF2)", c, e.state)
		}
		if e.state == covered {
			if e.witness == "" {
				t.Fatalf("claim %s is covered with NO witness — the witness is the test function whose cell the gate binds the evidence to; without it the anchors have no home", c)
			}
			if len(e.anchors) == 0 {
				t.Fatalf("claim %s is covered with NO anchors — an unanchored covered claim is an assertion the gate cannot check; the anchors are the machine-checked half of the claim", c)
			}
			for _, a := range e.anchors {
				if strings.TrimSpace(a) == "" {
					t.Fatalf("claim %s carries a BLANK anchor — a blank matches everything and therefore proves nothing (r2 MF3)", c)
				}
			}
		}
		claims = append(claims, c)
	}
	sort.Strings(claims)
	return claims
}

// witnessSpan locates the named test function by Go AST (r2 MF1 — function
// positions, not regex guessing) and returns its source span — doc comment
// through closing brace — plus its file. The span is the cell a covered
// claim owns: the citation AND every anchor must appear inside it. A
// citation that names the claim from an unrelated comment — same file or
// not — is not evidence, and neither is an anchor that matches outside the
// witness.
func witnessSpan(t *testing.T, fn string) (span, rel string) {
	t.Helper()
	const self = "matrix_coverage_test.go"
	root := repoRoot(t)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || span != "" {
			return nil
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") || filepath.Base(path) == self {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, src, parser.ParseComments)
		if perr != nil {
			return nil // an unparsable file fails the build on its own; not this gate's finding
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name == nil || fd.Name.Name != fn {
				continue
			}
			start := fd.Pos()
			if fd.Doc != nil {
				start = fd.Doc.Pos()
			}
			span = string(src)[fset.Position(start).Offset:fset.Position(fd.End()).Offset]
			rel, _ = filepath.Rel(root, path)
			return nil
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repo for the witness %s: %v", fn, err)
	}
	if span == "" {
		t.Fatalf("the witness %s is not a test function in this repo — a covered claim names a cell that must exist, or its evidence is a claim about nothing", fn)
	}
	return span, rel
}

// numericRows parses §2's numbered rows: "| 2.1a | ...". A row id that
// appears TWICE is fatal (r0 MF2): the original deduplication silently
// accepted an ambiguous identity — two normative rows answering to one id —
// and a gate that cannot tell them apart must not pick one and continue.
func numericRows(t *testing.T, section, body string) []string {
	t.Helper()
	seen := map[string]bool{}
	var out []string
	for _, ln := range strings.Split(body, "\n") {
		if m := matrixRowRe.FindStringSubmatch(ln); m != nil {
			if seen[m[1]] {
				t.Fatalf("§%s lists row %q twice — a duplicated row id is an ambiguous identity, and the gate refuses to deduplicate its way past it", section, m[1])
			}
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	if len(out) == 0 {
		t.Fatalf("parsed ZERO rows out of §%s: the table format changed and this gate is now measuring nothing", section)
	}
	return out
}

// citedRows scans every test in the repo for row citations.
//
// THIS FILE IS EXCLUDED, and the exclusion IS load-bearing — verified by
// drill: remove it and the gate reddens on its own prose. This file quotes
// PR #36's caps citation verbatim while explaining the case-insensitivity
// fix ("// MATRIX ROW 2.4"), and this very paragraph names an awaiting row
// in citation shape ("row 4:Sync"). Scan this file and both light up as
// self-citations, the gate counting its own
// description of the matrix as coverage of the matrix. The first version
// claimed the exclusion was not load-bearing and was right at the time —
// verified then by removing it and watching the gate still pass; the §2
// quote arrived with the case-insensitivity fix and the §3/§4/§5
// discussion widened the surface. No comment in this file may assume the
// scan cannot see it.
func citedRows(t *testing.T) map[string][]string {
	t.Helper()
	const self = "matrix_coverage_test.go"
	out := map[string][]string{}
	root := repoRoot(t)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") || filepath.Base(path) == self {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		for _, m := range citationRe.FindAllStringSubmatch(string(src), -1) {
			out[m[1]] = append(out[m[1]], rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repo: %v", err)
	}
	return out
}

// TestMatrixCoverage_EveryRowIsTriaged fails in three directions — plus the
// claim-level obligations (r0 MF1) and the parent-derivation consistency check.
func TestMatrixCoverage_EveryRowIsTriaged(t *testing.T) {
	rows := matrixRowIDs(t)
	rowSet := make(map[string]bool, len(rows))
	for _, r := range rows {
		rowSet[r] = true
	}
	claims := matrixClaimIDs(t, rowSet)
	claimCount := map[string]int{}
	for _, c := range claims {
		parent, _, _ := strings.Cut(c, "#")
		claimCount[parent]++
	}
	cited := citedRows(t)

	var untriaged, regressed, promotable, phantomTest []string
	var claimRegressed, claimPromotable, evidenceVanished, parentDisagrees []string

	for _, row := range rows {
		entry, known := matrixTriage[row]
		if !known {
			untriaged = append(untriaged, row)
			continue
		}
		// A claim-bearing parent is governed by its claims: its coverage is
		// derived (checked against the map below), its citations live at
		// claim level. Only a whole-row citation while the derivation says
		// awaiting is the parent's own failure — a claim the claims
		// themselves contradict.
		if claimCount[row] > 0 {
			if _, isCited := cited[row]; isCited && entry.state == awaiting {
				promotable = append(promotable, row+" — cited whole while its claims are still tracked per-claim ("+strings.Join(cited[row], ", ")+")")
			}
			continue
		}
		_, isCited := cited[row]
		switch entry.state {
		case covered:
			if !isCited {
				regressed = append(regressed, row+" ("+entry.reason+")")
			}
		case awaiting:
			if isCited {
				promotable = append(promotable, row+" — now cited by "+strings.Join(cited[row], ", "))
			}
		case uncited:
			if isCited {
				promotable = append(promotable, row+" — now cited by "+strings.Join(cited[row], ", ")+
					" (promote to covered)")
			}
			// The named cell must exist. An `uncited` claim is otherwise just
			// an assertion that something, somewhere, covers this.
			if !testFuncExists(t, entry.reason) {
				phantomTest = append(phantomTest, row+" names "+entry.reason+", which is not a test function in this repo")
			}
		}
	}

	// The claim loop. A covered claim owns a CELL (r2 MF1): the witness
	// function's AST span, doc comment through closing brace. The citation
	// naming the claim AND every anchor must appear inside that span — the
	// proof and the citation resolve to the same owned cell, and a fragment
	// that matches anywhere else, same file or not, is a comment, not
	// evidence. An awaiting claim cited anywhere follows the ordinary
	// promotion path.
	for _, claim := range claims {
		entry := claimTriage[claim]
		switch entry.state {
		case covered:
			span, _ := witnessSpan(t, entry.witness)
			if !citesRow(span, claim) {
				claimRegressed = append(claimRegressed, claim+" ("+entry.reason+") — its witness "+entry.witness+" does not cite it; the citation must live inside the same cell as the proof")
			}
			for _, a := range entry.anchors {
				if !strings.Contains(span, a) {
					evidenceVanished = append(evidenceVanished,
						claim+"'s anchor no longer appears inside its witness "+entry.witness+": "+a+
							" — the proof and the citation must resolve to the same owned cell; a match elsewhere, same file or not, is a comment, not evidence")
				}
			}
		case awaiting:
			if _, isCited := cited[claim]; isCited {
				claimPromotable = append(claimPromotable, claim+" — now cited by "+strings.Join(cited[claim], ", "))
			}
		}
	}

	// Parent-derivation consistency: a row with claims is covered exactly
	// when EVERY claim is exactly covered (r1 MF2) — anything else derives
	// awaiting. The hand-written parent state must AGREE with the
	// derivation, or the map is asserting something its own claims
	// contradict — in either direction.
	for parent, n := range claimCount {
		derived := covered
		for _, c := range claims {
			if p, _, _ := strings.Cut(c, "#"); p == parent && claimTriage[c].state != covered {
				derived = awaiting
			}
		}
		if matrixTriage[parent].state != derived {
			parentDisagrees = append(parentDisagrees, fmt.Sprintf(
				"%s is %s in matrixTriage but its %d claims derive %s — the parent state must agree with the claims or the map asserts what its own registry contradicts",
				parent, stateName(matrixTriage[parent].state), n, stateName(derived)))
		}
	}

	// 1. A row in the spec that nobody has triaged. The matrix grew and the
	//    suite did not notice.
	if len(untriaged) > 0 {
		t.Errorf("matrix rows with no entry in matrixTriage: %v\n"+
			"A row was added to the spec without deciding whether it is testable yet. "+
			"Add it as covered (with the cell that proves it) or awaiting (with the reason).",
			untriaged)
	}

	// 2. A row claimed covered that nothing cites any more. Someone deleted or
	//    renamed the cell and the claim outlived it.
	if len(regressed) > 0 {
		t.Errorf("rows marked covered but cited by no test: %v\n"+
			"Either the cell was removed, or its citation comment was lost in a refactor.",
			regressed)
	}

	// 3. A row marked awaiting that a test now cites. This is the good news
	//    case, and it still fails: the triage must be promoted so the list
	//    keeps meaning what it says.
	if len(promotable) > 0 {
		t.Errorf("rows marked awaiting that ARE now tested: %v\n"+
			"Promote them to covered — an awaiting list that lags reality stops being a to-do list and becomes a lie.",
			promotable)
	}

	if len(phantomTest) > 0 {
		t.Errorf("rows marked uncited whose named cell does not exist: %v\n"+
			"An uncited claim must name a real test, or it is an excuse rather than a record.",
			phantomTest)
	}

	if len(claimRegressed) > 0 {
		t.Errorf("claims marked covered but cited by no test: %v\n"+
			"A claim is a promise that evidence exists; the citation is how the scan sees it.",
			claimRegressed)
	}

	if len(claimPromotable) > 0 {
		t.Errorf("claims marked awaiting that ARE now tested: %v\n"+
			"Promote them — the obligation the claim records has been met.",
			claimPromotable)
	}

	if len(evidenceVanished) > 0 {
		t.Errorf("covered claims whose evidence no longer matches: %v\n"+
			"The anchors are the machine-checked half of a covered claim — the case that proved it was deleted or reworded while the claim still says covered.",
			evidenceVanished)
	}

	if len(parentDisagrees) > 0 {
		t.Errorf("parent rows whose state contradicts their claims: %v\n"+
			"A row with claims is covered exactly when every claim is covered; the hand-written parent state must agree with the derivation.",
			parentDisagrees)
	}

	t.Logf("matrix conformance: %d row keys across %d sections + %d prose units — "+
		"%d covered, %d tested-but-uncited, %d awaiting a cell; claims: %d covered / %d awaiting",
		len(rows), len(coveredSections), len(proseUnits), countState(covered), countState(uncited), countState(awaiting),
		countClaims(covered), countClaims(awaiting))
}

func countClaims(s rowState) int {
	n := 0
	for _, e := range claimTriage {
		if e.state == s {
			n++
		}
	}
	return n
}

func countState(s rowState) int {
	n := 0
	for _, e := range matrixTriage {
		if e.state == s {
			n++
		}
	}
	return n
}

func stateName(s rowState) string {
	switch s {
	case covered:
		return "covered"
	case awaiting:
		return "awaiting"
	case uncited:
		return "uncited"
	}
	return "?"
}

// citesRow reports whether the text contains a citation of the row or claim
// id in any of the shapes the citation convention uses — the same
// case-insensitivity citationRe carries, checked against a witness span.
func citesRow(text, id string) bool {
	re := regexp.MustCompile(`(?i)\browz?\s+` + regexp.QuoteMeta(id) + `\b`)
	return re.MatchString(text)
}

// TestMatrixCoverage_TriageHasNoPhantomRows guards the other direction: an
// entry in matrixTriage naming a row the matrix does not contain. That happens
// when a row is renumbered — or, for the named-key sections, when a row's
// first cell is reworded far enough to change its derived key — and it would
// otherwise sit here forever describing coverage of something that no longer
// exists. The prose units are guarded the same way by their anchors in
// matrixRowIDs: a vanished block fails there, loudly.
func TestMatrixCoverage_TriageHasNoPhantomRows(t *testing.T) {
	rows := matrixRowIDs(t)
	inMatrix := make(map[string]bool, len(rows))
	for _, r := range rows {
		inMatrix[r] = true
	}
	var phantom []string
	for row := range matrixTriage {
		if !inMatrix[row] {
			phantom = append(phantom, row)
		}
	}
	sort.Strings(phantom)
	if len(phantom) > 0 {
		t.Fatalf("matrixTriage names rows the matrix does not contain: %v\n"+
			"A renumbered, deleted, or reworded row leaves a claim behind that describes nothing. "+
			"Re-key the triage entry to the row's current identity.",
			phantom)
	}
}

// testFuncExists reports whether a Go test function of this name is declared
// anywhere in the repo, so an `uncited` entry names something real.
func testFuncExists(t *testing.T, name string) bool {
	t.Helper()
	if name == "" {
		return false
	}
	want := regexp.MustCompile(`func\s+` + regexp.QuoteMeta(name) + `\s*\(`)
	found := false
	_ = filepath.Walk(repoRoot(t), func(path string, info os.FileInfo, err error) error {
		if err != nil || found {
			return nil
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr == nil && want.Match(src) {
			found = true
		}
		return nil
	})
	return found
}

// TestMatrixCoverage_EverySectionIsCitable is the gate's check on ITSELF.
//
// citationRe once required a digit or a dot after the section number, so a row
// in §4a — written "4a:Parse" — could not be cited by any cell. All eight §4a
// rows were therefore STRUCTURALLY UNPROMOTABLE: no citation could exist, the
// "awaiting rows that ARE now tested" arm could never fire for them, and their
// reasons sat stale for as long as anyone cared to look. A gate everybody
// trusted had a section-sized hole, and the hole was silent in the direction of
// reporting no drift.
//
// So the pattern is now asserted against every section this gate claims, and a
// future section-naming scheme that citationRe cannot express fails HERE rather
// than becoming another quiet exemption. It is prove-the-instrument-observes
// turned on the instrument that watches the instruments: ask what input would
// make the guard complain, and confirm that input is expressible.
func TestMatrixCoverage_EverySectionIsCitable(t *testing.T) {
	for _, sec := range coveredSections {
		// The shape a cell writes: "// Witness for row <section>:<row-name>."
		probe := "// Witness for row " + sec.id + ":Example-Row_1."
		got := citationRe.FindStringSubmatch(probe)
		if got == nil {
			t.Errorf("section %q cannot be cited: citationRe does not match %q.\n"+
				"Every row in that section is unpromotable and this gate is blind to it — "+
				"widen the pattern rather than leaving the section silently ungated.", sec.id, probe)
			continue
		}
		if want := sec.id + ":Example-Row_1"; got[1] != want {
			t.Errorf("section %q cites as %q, want %q — a partial match citing the wrong key is worse "+
				"than none, because it credits a row nobody wrote a cell for", sec.id, got[1], want)
		}
	}

	// And the numeric claim form §2 uses ("row 2.4"), for the same reason.
	for _, sec := range coveredSections {
		if !sec.numeric {
			continue
		}
		probe := "// Witness for row " + sec.id + ".4"
		if citationRe.FindStringSubmatch(probe) == nil {
			t.Errorf("numeric section %q cannot be cited: citationRe does not match %q", sec.id, probe)
		}
	}
}
