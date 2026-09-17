// Package gatemutation holds the mutation controls as CODE, so they cannot
// quietly stop matching the tree they attack.
//
// WHY THIS EXISTS. A mutation control is a deliberate break: change one
// behaviour, run the test that claims to guard it, and require that test to
// fail. A test that stays green has named a guarantee it would not actually
// catch a violation of. That makes the set of controls load-bearing evidence —
// and until now it lived nowhere. Each run's controls were retyped into a
// runner's instructions from memory, which has all the failure modes of any
// unretained procedure, and one of them already happened: a control named a
// test that had been deleted, so it could not break anything, and the run
// scored it as INVALID only because the runner happened to check. Nothing else
// in the process would have noticed that a guarantee had stopped being proven.
//
// WHAT IS CHECKED IN HERE IS THE DEFINITION, NOT THE RUNNER. The edits are
// applied on a disposable copy by whatever executes the gate; this package
// exists so that the edits, the tests they target, and the reason each pairing
// matters are versioned alongside the code, and so that a control which has
// drifted fails a test HERE rather than being silently mis-run there.
package gatemutation

import "time"

// Mutation is one deliberate break and the cell that must notice it.
type Mutation struct {
	// Name is stable, because ledgers refer to these by name across runs.
	Name string
	// File is relative to the repository root.
	File string
	// Anchor must appear EXACTLY ONCE in File. Exactly once is the whole
	// safety property: an anchor matching twice would mutate something the
	// control never intended, and one matching zero times would be applied to
	// nothing while the run reported a verdict.
	Anchor string
	// Replacement is what Anchor becomes.
	Replacement string
	// Package is the package the cell lives in, as `go test` takes it. A bare
	// test name is ambiguous across packages, so a runner given only the name
	// has to guess -- and a guess that picks the wrong package runs nothing,
	// which reads exactly like a control that failed to discriminate.
	Package string
	// Test is the cell that must FAIL once the mutation is applied. It must
	// exist; a control naming an absent test proves nothing and has happened.
	Test string
	// Guarantee says, in one sentence, what goes unproven if this survives.
	// It is the sentence a reviewer reads when a control comes back green.
	Guarantee string
	// Fails is the exact assertion the cell must fail on: a substring of the
	// message that control is meant to provoke.
	//
	// WITHOUT IT, RED MEANS ONLY THAT THE FUNCTION FAILED. A neighbouring
	// subtest, a fixture panic or an unrelated assertion all produce
	// "--- FAIL: <Test>", and scoring those as proof credits a control for a
	// failure it did not cause. Two of the identity controls were doing
	// exactly that -- reddening through the check next door.
	Fails string
	// Timeout bounds the cell's run. Zero means the default.
	//
	// CARRIED PER CONTROL BECAUSE THE RIGHT BOUND IS A PROPERTY OF THE CELL,
	// not of the runner's mood on the day. A control whose cell detects a
	// break by waiting out a real bound needs a longer one; a control whose
	// cell should fail in milliseconds is better served by a short one, since
	// a generous timeout turns "the seam was bypassed" into "something hung".
	Timeout time.Duration
	// Race runs the cell under the detector.
	//
	// FOR CONTROLS WHOSE BREAK IS A RACE AND NOTHING ELSE. A lock released one
	// line too early changes no result, returns no error and fails no
	// assertion; the only witness is the detector. Without this the control
	// would be applied, the cell would pass, and the runner would score it
	// GREEN -- reporting that a guarantee is unproven when in truth the run
	// was never equipped to see it. Off by default because -race costs about
	// ten times the wall clock and most controls have a real assertion to fail
	// on.
	Race bool
	// Count repeats the cell. Non-zero only where a single run would be a
	// lottery -- see the cancellation control, which was once green 249 times
	// in 250 and is now deterministic but still worth repeating.
	Count int
}

// All returns every control, in ledger order.
//
// ORDERED AND NAMED rather than discovered, because a ledger has to be
// comparable between runs: "control 6 survived" must mean the same thing in
// two ledgers a week apart.
func All() []Mutation {
	return []Mutation{
		{
			Name: "serve-line-at-enqueue", Package: "./core/exec/", File: "core/exec/scheduler.go",
			Anchor:      "\tr.serveLine()\n\tif w.state == waitResolved {",
			Replacement: "\tif w.state == waitResolved {",
			Test:        "TestScheduler_AnEligibleNewcomerIsServedAtEnqueueTime",
			// CONTAINMENT, NOT THE ORACLE. The cell owns a two-second
			// assertion and fails on it; this bound only stops a runaway. It
			// was 150s, which let a control wait ten times longer than the
			// policy it is meant to enforce and made the ledger unreplayable
			// from the definition.
			Timeout: 15 * time.Second,
			Fails:   "a request for a FREE target waited behind the line",
			Guarantee: "that a request whose own target is free is served when it arrives, " +
				"rather than waiting for an unrelated release that may never come",
		},
		{
			Name: "transaction-bound-is-a-deadline", Package: "./core/exec/", File: "core/exec/session.go",
			Anchor:      "\t\tif now.Before(until) {",
			Replacement: "\t\tif true || now.Before(until) {",
			Test:        "TestScheduler_AnExpiredTransactionIsNotAReasonToRefuse",
			Fails:       "was read as capacity that is",
			Guarantee: "that a transaction past its bound counts as capacity that IS coming, " +
				"so a request is not told nothing is coming moments before it arrives",
		},
		{
			Name: "cancellation-undoes-its-admission", Package: "./core/exec/", File: "core/exec/scheduler.go",
			Anchor:      "\tif err := <-w.done; err == nil {\n\t\tr.remove(w.s)\n\t}",
			Replacement: "\t_ = w",
			Test:        "TestScheduler_ACancellationThatLosesToAGrantUndoesTheAdmission",
			Count:       20,
			Fails:       "after a cancelled request, want 0",
			Guarantee: "that an admission granted in the instant a caller gives up is handed " +
				"back, rather than leaking a session nothing is coming to close",
		},
		{
			Name: "line-skips-the-ineligible", Package: "./core/exec/", File: "core/exec/scheduler.go",
			Anchor:      "\t\t\t\tw.blockedBy = err\n\t\t\t\tcontinue",
			Replacement: "\t\t\t\tw.blockedBy = err\n\t\t\t\tbreak",
			Test:        "TestScheduler_AFullTargetDoesNotBlockTheRestOfTheLine",
			Fails:       "blocked a waiter for a target with capacity",
			Guarantee: "that one saturated target cannot hold up every request for every " +
				"other target",
		},
		{
			Name: "timeout-unwraps-alone", Package: "./core/exec/", File: "core/exec/scheduler.go",
			Anchor:      "func (e *QueueTimeoutError) Unwrap() error { return ErrQueueTimeout }",
			Replacement: "func (e *QueueTimeoutError) Unwrap() []error { return []error{ErrQueueTimeout, e.blockedBy} }",
			Test:        "TestAdmissionWait_TheBlockingCapIsDiagnosisAndNotAnIdentity",
			Fails:       "an expired wait identifies as a lease-cap refusal",
			Guarantee: "that the blocking cap stays diagnosis rather than becoming an identity " +
				"an ordered switch can select, which would make the queue-timeout answer " +
				"unreachable in production while still registered",
		},
		{
			Name: "server-wait-uses-its-seam", Package: "./core/exec/", File: "core/exec/scheduler.go",
			Anchor:      "\t\ttimer := r.serverTimer(queueWait)",
			Replacement: "\t\ttimer := time.NewTimer(queueWait)",
			Test:        "TestScheduler_TheServerWaitExpiresWithItsOwnIdentity",
			Fails:       "never armed through the injected timer",
			Guarantee: "that the server's wait is armed through the injectable seam, without " +
				"which a gate silently becomes a multi-minute one and eventually reads as a hang",
		},
		{
			Name: "the-caller-owns-an-exact-tie", Package: "./core/exec/", File: "core/exec/scheduler.go",
			Anchor:      "\tif d, ok := ctx.Deadline(); !ok || d.After(serverDeadline) {",
			Replacement: "\tif d, ok := ctx.Deadline(); !ok || !d.Before(serverDeadline) {",
			Test:        "TestScheduler_OneBoundOwnsTheWaitDeterministically",
			Fails:       "which bound owns the wait must not",
			Guarantee: "that which bound owns a wait is decided by policy rather than by which " +
				"channel the runtime happens to see first",
		},
		{
			Name: "release-serves-the-line", Package: "./core/exec/", File: "core/exec/session.go",
			Anchor:      "\t\tr.serveLine()\n\t}\n\tr.mu.Unlock()",
			Replacement: "\t}\n\tr.mu.Unlock()",
			Test:        "TestScheduler_AReleaseNeverLeavesAnAdmittableWaiterWaiting",
			Fails:       "capacity was free and",
			Guarantee: "that freed capacity never sits idle while somebody able to use it is " +
				"still queued, which is what makes joining the line safe rather than a disadvantage",
		},
		{
			Name: "expired-wait-names-its-blocker", Package: "./core/exec/", File: "core/exec/wire_session.go",
			// Follows the code: the classifier now returns a reason and its
			// detail rather than a finished denial, because stamping a refusal
			// as disclosable belongs at the site that verified the credential.
			Anchor:      "\t\treturn DenyQueueTimeout, d, true",
			Replacement: "\t\treturn DenyQueueTimeout, \"\", true",
			Test:        "TestOpenWireSession_AWaitThatExpiresIsRecordedAsAWaitNotAsACapRefusal",
			Fails:       "want it to record that the request waited",
			Guarantee: "that the one record of an expired wait says which limit to raise, not " +
				"merely that somebody waited",
		},
		{
			Name: "the-wait-arm-comes-first", Package: "./core/exec/", File: "core/exec/wire_session.go",
			Anchor: "\tcase errors.Is(rerr, ErrQueueTimeout):",
			// THE REPLACEMENT RETURNS admissionAnswer's OWN TUPLE, and that is
			// not a detail. It used to call denyAfterAuthorization, which
			// returns an error -- correct when the control was written, wrong
			// the moment admissionAnswer's signature changed to
			// (reason, detail string, ok bool). A mutation that does not
			// compile is scored INVALID, so this control was unscorable on
			// every head from that day until it was noticed, and silently: a
			// guarantee nobody was proving, sitting in a list of guarantees.
			Replacement: "\tcase errors.Is(rerr, ErrLeaseCapExceeded):\n\t\treturn DenyLeaseCap, \"\", true\n\tcase errors.Is(rerr, ErrQueueTimeout):",
			Test:        "TestAdmissionDenial_TheWaitOutranksTheCapItWaitedOn",
			Fails:       "was recorded as one refused on arrival",
			Guarantee: "that a request which waited is never recorded as one refused on arrival, " +
				"independently of what the error happens to unwrap to",
		},
		{
			Name: "coordinates-come-from-the-code", Package: "./internal/gatematrix/", File: "internal/gatematrix/coords.go",
			Anchor:      "\tlines := strings.Split(doc, \"\\n\")",
			Replacement: "\tif true {\n\t\treturn doc\n\t}\n\tlines := strings.Split(doc, \"\\n\")",
			Test:        "TestCoordinates_AMovedUseIsCorrected",
			Fails:       "the declaration was not located",
			Guarantee: "that the generator reads the tree rather than echoing back the document " +
				"it was handed",
		},
		{
			Name: "the-matrix-is-current", Package: "./internal/gatematrix/", File: "docs/admission-gate-matrix.md",
			// A UNIQUE coordinate, not a shared prefix. The first version used
			// "decl session.go:", which matches four rows — so the control
			// would have rewritten three rows it never meant to touch. Caught
			// by this package's own anchor cell on its first run, which is the
			// argument for having the controls checked in.
			Anchor:      "decl cancel_registry.go:138",
			Replacement: "decl cancel_registry.go:999999",
			Test:        "TestCoordinates_TheMatrixIsWhatTheGeneratorWouldWrite",
			Fails:       "the matrix's coordinates are not what the code says",
			Guarantee: "that a coordinate drifting out of step with the code is caught rather " +
				"than discovered later by a downstream walk",
		},
		// ---- L6b: demand reclamation ----
		//
		// Every control below attacks a path that ENDS SOMEBODY'S SESSION. The
		// cost of one of these being wrong is not a failed request; it is a
		// developer disconnected for a reason that was not true, or a lease
		// held for the life of the process by a session that serves nobody.
		{
			Name: "demand-is-wired-to-the-scheduler", Package: "./core/exec/",
			File:        "core/exec/engine.go",
			Anchor:      "\te.sessions.onDemand = e.demandReclaim",
			Replacement: "\te.sessions.onDemand = nil",
			Test:        "TestDemandReclaim_TheEngineWiresItToTheScheduler",
			Fails:       "did not install its reclaimer on the registry",
			Guarantee: "that demand reclamation is reachable from the scheduler at all, " +
				"without which every part of it is correct and none of it runs",
		},
		{
			Name: "only-an-untroubled-holder-is-chosen", Package: "./core/exec/",
			File:        "core/exec/demand_reclaim.go",
			Anchor:      "\t\teligible := s.wire && !s.busy && s.tx == nil && s.recvToken != 0 && s.wake != nil",
			Replacement: "\t\teligible := s.wire",
			Test:        "TestDemandReclaim_OnlyAnUntroubledIdleHolderIsChosen",
			Fails:       "a holder was chosen while",
			Guarantee: "that a session running a statement, holding a transaction, not " +
				"listening, or unreachable is never terminated for capacity",
		},
		{
			Name: "predicate-and-reservation-are-one-hold", Package: "./core/exec/",
			File:        "core/exec/demand_reclaim.go",
			Anchor:      "\t\treserved := claimed && s.beginCloseLocked(\"\", ReasonDemandReclaimed)",
			Replacement: "\t\ts.mu.Unlock()\n\t\ts.mu.Lock()\n\t\treserved := claimed && s.beginCloseLocked(\"\", ReasonDemandReclaimed)",
			Test:        "TestDemandReclaim_NothingCanSlipBetweenJudgingAndClaiming",
			Fails:       "the session's lock is released between judging it idle and claiming it",
			Guarantee: "that a session cannot become active between being judged idle and " +
				"being claimed, and so be terminated after it started work",
		},
		{
			Name: "the-longest-silent-holder-is-chosen", Package: "./core/exec/",
			File:        "core/exec/demand_reclaim.go",
			Anchor:      "\t\treturn candidates[i].idleFor(now) > candidates[j].idleFor(now)",
			Replacement: "\t\treturn candidates[i].idleFor(now) < candidates[j].idleFor(now)",
			Test:        "TestDemandReclaim_TheLongestSilentHolderIsChosen",
			Fails:       "want the holder that had been silent longest",
			Guarantee: "that the session ended is the one whose owner is least likely to " +
				"notice, rather than somebody who paused for a moment",
		},
		{
			Name: "a-stale-generation-is-refused", Package: "./core/exec/",
			File:        "core/exec/demand_reclaim.go",
			Anchor:      "\tif !ok || s.gen != gen {\n\t\treturn nil, false\n\t}",
			Replacement: "\tif !ok {\n\t\treturn nil, false\n\t}",
			Test:        "TestDemandReclaim_AStaleGenerationCannotEndAReplacementSession",
			Fails:       "resolved to the session that replaced it",
			Guarantee: "that a notice about one session cannot end whichever session replaced " +
				"it, disconnecting somebody who was never selected",
		},
		{
			Name: "a-stale-receive-token-is-refused", Package: "./core/exec/",
			File:        "core/exec/demand_reclaim.go",
			Anchor:      "\tif s.recvToken != token {",
			Replacement: "\tif false {",
			Test:        "TestDemandReclaim_AStaleReceiveTokenIsIgnored",
			Fails:       "a stale token took the notice",
			Guarantee: "that one read's outcome cannot close a later read's window and consume " +
				"a notice meant for it",
		},
		{
			Name: "no-offer-without-a-knock", Package: "./core/exec/",
			File:        "core/exec/demand_reclaim.go",
			Anchor:      "\tif s.wake == nil {",
			Replacement: "\tif false {",
			Test:        "TestDemandReclaim_NoOfferIsIssuedWithoutAKnock",
			Fails:       "an offer was issued as token",
			Guarantee: "that a session never advertises itself as reclaimable when nothing " +
				"can wake its owner, which would strand the lease",
		},
		{
			Name: "the-record-keeps-selection-time-state", Package: "./core/exec/",
			File:        "core/exec/demand_reclaim.go",
			Anchor:      "\t\t\ts.demandHeldObjects = s.holdsObjects()",
			Replacement: "\t\t\ts.demandHeldObjects = false",
			Test:        "TestDemandReclaim_AHolderOfObjectsIsEndedAndTheRecordSaysSo",
			Fails:       "notice.HeldObjects = false",
			Guarantee: "that a holder of objects is recorded as one, so an operator can tell " +
				"clients leaving statements open from idle connections nobody closed",
		},
		{
			Name: "the-knock-spares-the-write-deadline", Package: "./frontdoor/",
			File:        "frontdoor/demand_wake.go",
			Anchor:      "\t\tif err := conn.SetReadDeadline(now().Add(-time.Second)); err != nil {",
			Replacement: "\t\tif err := conn.SetDeadline(now().Add(-time.Second)); err != nil {",
			Test:        "TestDemandKnock_TouchesTheReadDeadlineOnly",
			Fails:       "a blocked Receive is not interrupted",
			Guarantee: "that waking the owner does not fail the very frame it is waking it to " +
				"write, turning an explained ending into a silent disconnection",
		},
		{
			Name: "the-offer-is-retired-at-the-wait", Package: "./frontdoor/",
			File:        "frontdoor/session_loop.go",
			Anchor:      "\t\tif n, woken := owner.retire(); woken {",
			Replacement: "\t\tif n, woken := (exec.DemandNotice{}), false; woken {",
			Test:        "TestDrivenDemand_AnIdleClientIsToldBeforeTheConnectionEnds",
			Fails:       "want \"57P01\"",
			Guarantee: "that a notice delivered while the loop waited is acted on, rather than " +
				"leaving a reserved session with nobody to end it",
		},
		{
			Name: "finalisation-is-total", Package: "./frontdoor/",
			File:        "frontdoor/demand_wake.go",
			Anchor:      "\t\to.dr.FinishDemandReclaim(ctx, n.ID, n.Gen, delivery)",
			Replacement: "\t\tif delivery != exec.DemandNotAttempted {\n\t\t\to.dr.FinishDemandReclaim(ctx, n.ID, n.Gen, delivery)\n\t\t}",
			Test:        "TestDrivenDemand_AnUndeclaredOutcomeStillReleasesTheLease",
			Fails:       "the session was never finalised",
			Guarantee: "that a reserved session is ended on every path, including the one " +
				"where nothing could be sent -- otherwise its lease is held forever",
		},
		{
			Name: "the-frame-precedes-the-release", Package: "./frontdoor/",
			File:        "frontdoor/demand_wake.go",
			Anchor:      "\t\tdelivery = exec.DemandDelivered",
			Replacement: "\t\tdelivery = exec.DemandFlushFailed",
			Test:        "TestDrivenDemand_TheReleaseHappensPromptlyAfterTheFlush",
			Fails:       "though the client was reading",
			Guarantee: "that the record distinguishes a client that was told from one that " +
				"was not, which is the only evidence anyone was told at all",
		},
		{
			Name: "reclamation-is-a-control-not-a-refusal", Package: "./frontdoor/",
			File:        "frontdoor/demand_terminal.go",
			Anchor:      "\t\tKind:   outcome.Control,",
			Replacement: "\t\tKind:   outcome.Refusal,",
			Test:        "TestDemandReclaimed_ItsOutcomeIsNotAHeldObjectCondition",
			Fails:       "want Control: nobody was refused",
			Guarantee: "that a session we chose to end is not counted among requests we " +
				"refused, which are different numbers with different remedies",
		},
		{
			Name: "reclamation-is-charged-to-nobody", Package: "./frontdoor/",
			File:        "frontdoor/demand_terminal.go",
			Anchor:      "\t\tCharge: outcome.NotApplicable,",
			Replacement: "\t\tCharge: outcome.Credential,",
			Test:        "TestDemandReclaimed_ItsOutcomeIsNotAHeldObjectCondition",
			Fails:       "want NotApplicable",
			Guarantee: "that ending somebody's session for capacity never charges their " +
				"credential throttle, which would ban them for our decision",
		},
		{
			Name: "identity-excludes-git", Package: "./internal/gateidentity/", File: "internal/gateidentity/identity.go",
			Anchor:      "\t\".git\": true, \"node_modules\": true, \".cache\": true,",
			Replacement: "\t\"node_modules\": true, \".cache\": true,",
			Test:        "TestIdentity_AWorktreeGitFileIsNotPartOfTheFingerprint",
			Fails:       "the fingerprint changed when a worktree's .git FILE appeared",
			Guarantee: "that a worktree and its .git-excluded copy can be compared at all, " +
				"without which the identity gate rejects every honest copy and teaches " +
				"everyone to ignore it",
		},
		// ---- the command boundary ----
		//
		// The helpers are controlled above; these prove the EXECUTABLE actually
		// asks them. A library that refuses correctly behind a command that
		// never calls it is a hole in the one place it does not show, and every
		// library-level control stays RED while the gate script sees nothing.
		{
			Name: "cli-guards-the-recorded-manifest", Package: "./internal/gateidentity/cmd/identity/",
			File:        "internal/gateidentity/cmd/identity/main.go",
			Anchor:      "\tfor _, ev := range []struct{ flag, path string }{{\"-manifest\", *manifest}, {\"-against\", *against}} {",
			Replacement: "\tfor _, ev := range []struct{ flag, path string }{{\"-against\", *against}} {",
			Test:        "TestCLI_RecordingIntoTheRootIsRefused",
			Fails:       "for a manifest inside the root, want 2",
			Guarantee: "that the command checks the RECORDED manifest's path, not only the " +
				"one it compares against -- writing evidence into the tree changes what it records",
		},
		{
			Name: "cli-guards-the-against-manifest", Package: "./internal/gateidentity/cmd/identity/",
			File:        "internal/gateidentity/cmd/identity/main.go",
			Anchor:      "{\"-manifest\", *manifest}, {\"-against\", *against}} {",
			Replacement: "{\"-manifest\", *manifest}} {",
			Test:        "TestCLI_CheckingAgainstAManifestInTheRootIsRefused",
			Fails:       "for an against-manifest inside the root, want 2",
			Guarantee: "that the command checks the manifest it compares against, without which " +
				"an inside-root evidence file is read as an ordinary difference",
		},
		{
			Name: "cli-refuses-two-authorities", Package: "./internal/gateidentity/cmd/identity/",
			File:        "internal/gateidentity/cmd/identity/main.go",
			Anchor:      "\tif *expect != \"\" && *against != \"\" {",
			Replacement: "\tif false {",
			Test:        "TestCLI_ExpectAndAgainstTogetherAreRefused",
			Fails:       "must not both be accepted",
			Guarantee: "that a manifest and a flag claiming different identities cannot both be " +
				"accepted, which lets the check pass while the record describes something else",
		},
		{
			Name: "cli-validates-the-head", Package: "./internal/gateidentity/cmd/identity/",
			File:        "internal/gateidentity/cmd/identity/main.go",
			Anchor:      "\tif *head != \"\" && !isHex40(*head) {",
			Replacement: "\tif false {",
			Test:        "TestCLI_ShortHeadIsRefused",
			Fails:       "short head exited",
			Guarantee: "that a short or malformed commit id is refused, so a manifest cannot " +
				"record a provenance nobody can resolve",
		},
		{
			Name: "cli-validates-the-base", Package: "./internal/gateidentity/cmd/identity/",
			File:        "internal/gateidentity/cmd/identity/main.go",
			Anchor:      "\tif *base != \"\" && !isHex40(*base) {",
			Replacement: "\tif false {",
			Test:        "TestCLI_ShortBaseIsRefused",
			Fails:       "short base exited",
			Guarantee: "that a malformed merge base is refused on its own route, not merely " +
				"because the head happened to be checked first",
		},
		{
			Name: "cli-refuses-a-forged-note", Package: "./internal/gateidentity/cmd/identity/",
			File:        "internal/gateidentity/cmd/identity/main.go",
			Anchor:      "\tif strings.ContainsAny(*note, \"\\r\\n\") {",
			Replacement: "\tif false {",
			Test:        "TestCLI_NoteWithLineBreakIsRefused",
			Fails:       "newline note exited",
			Guarantee: "that a note cannot inject a line into a line-oriented manifest, and so " +
				"cannot forge a digest header of its own choosing",
		},
		{
			Name: "identity-requires-a-digest", Package: "./internal/gateidentity/",
			File:        "internal/gateidentity/identity.go",
			Anchor:      "\tif digest == \"\" {",
			Replacement: "\tif false {",
			Test:        "TestIdentity_AManifestWithNoDigestIsRefused",
			Fails:       "carries no digest",
			Guarantee: "that a manifest with no digest is refused rather than compared " +
				"against nothing and reported as a pass",
		},
		{
			Name: "identity-verifies-the-body", Package: "./internal/gateidentity/",
			File: "internal/gateidentity/identity.go",
			// Keeps the binding used, so the mutated tree still compiles: a
			// replacement that fails to build is scored INVALID and proves
			// nothing, which the runner correctly refused to hide.
			Anchor:      "\tif got := DigestOf(entries); got != digest {",
			Replacement: "\tif got := DigestOf(entries); len(got) < 0 {",
			Test:        "TestIdentity_AManifestWithAnEditedBodyIsRefused",
			Fails:       "edited under an untouched header was accepted",
			Guarantee: "that a manifest's header is re-derived from its own entries, without " +
				"which a body edited under an untouched header reads as authoritative",
		},
		{
			Name: "identity-refuses-an-empty-manifest", Package: "./internal/gateidentity/",
			File:        "internal/gateidentity/identity.go",
			Anchor:      "\tif len(entries) == 0 {",
			Replacement: "\tif false {",
			Test:        "TestIdentity_AnEmptyManifestIsRefused",
			Fails:       "want it to say the manifest pins nothing",
			// HONEST ABOUT WHAT THIS PROVES. An empty list still hashes to
			// something, so the body check would refuse this manifest anyway --
			// and report it as EDITED, sending somebody to look for tampering
			// when the truth is that it describes nothing at all. What this
			// control holds is that the refusal names its own cause.
			Guarantee: "that an empty manifest is refused for describing nothing, rather than " +
				"being reported as tampered with and sending somebody after the wrong fault",
		},
		{
			Name: "identity-keeps-evidence-outside-the-root", Package: "./internal/gateidentity/",
			File:        "internal/gateidentity/identity.go",
			Anchor:      "\treturn fmt.Errorf(\"%w: %s is inside %s\", ErrInsideRoot, absEv, absRoot)",
			Replacement: "\treturn nil",
			Test:        "TestIdentity_EvidenceInsideTheRootIsRefused",
			Fails:       "was accepted; writing there changes the tree being fingerprinted",
			Guarantee: "that a manifest cannot be written into the tree it fingerprints, which " +
				"changes the thing it records and makes an exact copy read as changed",
		},
		{
			Name: "identity-refuses-two-authorities", Package: "./internal/gateidentity/",
			File:        "internal/gateidentity/identity.go",
			Anchor:      "\tif headers > 1 {",
			Replacement: "\tif false {",
			Test:        "TestIdentity_AManifestWithTwoDigestHeadersIsRefused",
			Fails:       "two digest headers was accepted",
			Guarantee: "that one manifest asserts exactly one identity, so appending a line " +
				"cannot change what a record claims",
		},
		{
			// THESE TWO CONTROLS ATTACK CELLS, NOT PRODUCTION CODE, because
			// for these two the cell IS the thing that was wrong. Both were
			// green on a developer's worktree and red in the sanctioned
			// container, which is the only place their verdict counts. A fix
			// to a cell needs the same evidence as a fix to anything else:
			// put the defect back, and require the cell to notice.
			Name: "source-tree-refusal-borrows-its-precondition", Package: "./internal/gatemutation/",
			File:   "internal/gatemutation/runner_meta_test.go",
			Anchor: "\tout, code := runRunner(t, bin, repo, \"-root\", dir, \"-only\", \"identity-excludes-git\")",
			Replacement: "\t_ = dir\n" +
				"\tout, code := runRunner(t, bin, repo, \"-root\", repo, \"-only\", \"identity-excludes-git\")",
			Test:  "TestRunner_ASourceTreeIsRefused",
			Fails: "want 2",
			Guarantee: "that the source-tree refusal builds the .git it needs instead of " +
				"borrowing one from wherever it happens to run, so it proves the runner's " +
				"refusal rather than the shape of the checkout",
		},
		{
			Name: "containment-probe-trusts-signal-zero", Package: "./internal/gatemutation/cmd/mutate/",
			File:        "internal/gatemutation/cmd/mutate/containment_test.go",
			Anchor:      "\treturn len(fields) > 0 && fields[0] == \"Z\", nil",
			Replacement: "\t_ = fields\n\treturn false, nil",
			Test:        "TestRunBounded_KillsTheWholeProcessTree",
			// ONLY DISCRIMINATES WHERE PID 1 DOES NOT REAP -- which is the
			// sanctioned container, and is the whole reason the defect existed.
			Timeout: 90 * time.Second,
			Fails:   "survived the containment timeout",
			Guarantee: "that a killed descendant nobody has reaped yet is read as dead, so " +
				"the cell reports on containment rather than on whether pid 1 calls wait",
		},
		{
			Name: "demand-retries-when-a-holder-becomes-askable", Package: "./core/exec/",
			File:        "core/exec/demand_reclaim.go",
			Anchor:      "\te.sessions.pressDemand(target)",
			Replacement: "\t_ = target",
			Test:        "TestDemandRetry_AHolderThatBecomesAskableServesTheRequestThatAlreadyAsked",
			// The cell waits five seconds for a knock that never comes.
			Timeout: 60 * time.Second,
			Fails:   "no holder was asked to leave when one became askable",
			Guarantee: "that a request already queued is answered when a holder becomes " +
				"reclaimable, rather than waiting out its whole bound beside one, because " +
				"a wire lease is never released on its own",
		},
		{
			Name: "a-departed-request-leaves-no-demand", Package: "./core/exec/",
			File:        "core/exec/scheduler.go",
			Anchor:      "\tw.wantsDemand = false\n\tr.dropDemand(w.leaseConn)",
			Replacement: "\tw.wantsDemand = false",
			Test:        "TestDemandRetry_ACancelledRequestLeavesNoDemandBehind",
			Fails:       "still reports unanswered demand",
			Guarantee: "that a request leaving the line gives its demand claim back, so the " +
				"next holder to go idle is not ended for somebody who has gone",
		},
		{
			Name: "an-offer-covers-only-a-wait", Package: "./frontdoor/",
			File:        "frontdoor/session_loop.go",
			Anchor:      "\t\toffered := !framed && !mid",
			Replacement: "\t\toffered := true",
			Test:        "TestDemandOfferWindow_NoOfferCoversAFrameTheClientAlreadyWon",
			Timeout:     180 * time.Second,
			Fails:       "published a receive offer with framed=",
			Guarantee: "that a receive offer is published only when the wait genuinely " +
				"begins between messages, so demand cannot reserve a session over a frame " +
				"the client had already won and discard it",
		},
		{
			Name: "a-finalisation-is-consumed-not-just-checked", Package: "./core/exec/",
			File:        "core/exec/demand_reclaim.go",
			Anchor:      "\ts.demandFinal = false\n",
			Replacement: "",
			Test:        "TestDemandFinalisation_ManyCallersPresentingOneNoticeYieldOneOwner",
			Fails:       "callers were told they owned the teardown",
			Guarantee: "that one reservation finalises once however many callers present " +
				"its notice, so one ending is not written twice into the trail and one " +
				"lease is not released twice",
		},
		{
			Name: "a-finalisation-checks-the-ending-it-claims", Package: "./core/exec/",
			File:        "core/exec/demand_reclaim.go",
			Anchor:      "!strings.HasPrefix(s.closeWhy, ReasonDemandReclaimed) {",
			Replacement: "!strings.HasPrefix(s.closeWhy, \"\") {",
			Test:        "TestDemandFinalisation_ItIsRefusedAgainstAnEndingItDoesNotOwn",
			Fails:       "does not own the ending",
			Guarantee: "that a stale notice cannot write a reclamation over a session an " +
				"operator or its own client ended, replacing a true record with a false one",
		},
		{
			Name: "a-demand-unit-is-spent-where-it-is-checked", Package: "./core/exec/",
			File:   "core/exec/demand_reclaim.go",
			Anchor: "\tif r.demandWanted[leaseConn] <= len(r.demandPromised[leaseConn]) {\n\t\treturn false\n\t}\n",
			// Unconditional promise: exactly the shape the defect had, and it
			// compiles, which a bare deletion of the whole function would not.
			Replacement: "",
			Test:        "TestDemandRetry_ConcurrentOffersSpendOneWaiterOnce",
			Fails:       "sessions reserved for one waiter after concurrent offers",
			Guarantee: "that one queued request spends one demand unit however many offers " +
				"open at once, so it cannot end several healthy sessions to answer itself",
		},
		{
			Name: "pressure-hysteresis-is-two-numbers", Package: "./core/pressure/",
			File:        "core/pressure/pressure.go",
			Anchor:      "\tclearPercent = 70",
			Replacement: "\tclearPercent = 80",
			Test:        "TestPressure_AHoveringFigureRaisesOnceAndClearsOnce",
			Fails:       "raises=",
			Guarantee:   "that a figure hovering at its threshold produces one raise and one clear rather than an event on every tick, which people filter and then stop reading",
		},
		{
			Name: "pressure-a-clear-carries-its-threshold", Package: "./core/pressure/",
			File:        "core/pressure/pressure.go",
			Anchor:      "\tcase Occupancy:\n\t\treturn r.Cap * clearPercent / 100",
			Replacement: "\tcase Occupancy:\n\t\treturn 0",
			Test:        "TestPressure_TheOccupancyBoundaryIsExact",
			Fails:       "reported clear threshold",
			Guarantee:   "that the number beside the figure is right at both ends, because an operator reads it to know how much room came back",
		},
		{
			Name: "pressure-classes-stay-apart", Package: "./core/pressure/",
			File:        "core/pressure/pressure.go",
			Anchor:      "out = append(out, Event{ID: r.ID, Class: r.Class, Kind: r.Kind,\n\t\t\t\tEntered: true,",
			Replacement: "out = append(out, Event{ID: r.ID, Class: Capacity, Kind: r.Kind,\n\t\t\t\tEntered: true,",
			Test:        "TestPressure_TheTwoClassesRaiseAndClearIndependently",
			Fails:       "did not raise as credential",
			Guarantee:   "that a throttled source is never reported as capacity, which would send an operator to resize a pool over somebody guessing passwords",
		},
		{
			Name: "pressure-a-vanished-subject-still-clears", Package: "./core/pressure/",
			File:        "core/pressure/pressure.go",
			Anchor:      "\t\tdelete(t.raised, id)\n\t\tout = append(out, Event{ID: id, Class: sig.Class, Kind: sig.Kind,\n\t\t\tEntered: false, Value: 0, Threshold: 0})",
			Replacement: "\t\t_ = sig",
			Test:        "TestPressure_ASignalWhoseSubjectIsGoneIsCleared",
			Fails:       "want one clear for the signal whose subject vanished",
			Guarantee:   "that every enter has its clear even when the user disconnected or the throttle expired, so a view never shows a subject nobody has seen for an hour",
		},
		{
			Name: "pressure-a-straddling-burst-sums", Package: "./core/pressure/",
			File:        "core/pressure/window.go",
			Anchor:      "\tif w.at[slot] != cur {\n\t\t// The slot belonged to an earlier window: it is reused, not added to.\n\t\tw.counts[slot] = 0\n\t\tw.at[slot] = cur\n\t}",
			Replacement: "\tw.counts[slot] = 0\n\tw.at[slot] = cur",
			Test:        "TestWindow_ABurstAcrossABoundarySums",
			Fails:       "a burst spanning two buckets totalled",
			Guarantee:   "that a burst crossing a bucket boundary is summed rather than reset, since denials do not wait for a boundary",
		},
		{
			Name: "pressure-the-window-slides", Package: "./core/pressure/",
			File: "core/pressure/window.go",
			// PIN EVERY INSTANT TO ONE BUCKET, so nothing ever ages out. The
			// obvious mutation -- narrowing the window to the current bucket --
			// breaks the cell's FIRST assertion instead of the sliding one it
			// names, which the runner correctly scores INVALID. A control has
			// to fail on the assertion it claims or it is proving something
			// else and saying otherwise.
			Anchor:      "func index(t time.Time) int64 { return t.UnixNano() / int64(bucketSpan) }",
			Replacement: "func index(t time.Time) int64 { return 0 }",
			Test:        "TestWindow_TheOldestBucketFallsOutFirst",
			Fails:       "the window must slide by a",
			Guarantee:   "that the window moves a bucket at a time rather than discarding everything, so a total does not fall off a cliff",
		},
		{
			Name: "pressure-a-read-evicts", Package: "./core/pressure/",
			File:        "core/pressure/window.go",
			Anchor:      "func (w *rateWindow) total(now time.Time) int {\n\tw.advance(now)",
			Replacement: "func (w *rateWindow) total(now time.Time) int {",
			Test:        "TestWindow_ItReturnsToZeroWithoutAnyFurtherWrites",
			Fails:       "still holds",
			Guarantee:   "that slots are reclaimed by the tick that reads them, so a window nobody writes to any more does not hold a burst from an hour ago forever",
		},
		{
			Name: "pressure-dimensions-are-capped", Package: "./core/pressure/",
			File:        "core/pressure/dimension.go",
			Anchor:      "\tfor len(d.seen) > MaxSubjects {",
			Replacement: "\tfor false {",
			Test:        "TestDimension_ItStaysBoundedUnderAFloodOfSubjects",
			Fails:       "subjects after 500 arrived",
			Guarantee:   "that a dimension keyed by something the peer chooses stays bounded, because an uncapped map keyed by source address is a memory exhaustion primitive",
		},
		{
			Name: "pressure-the-recent-survive", Package: "./core/pressure/",
			File:        "core/pressure/dimension.go",
			Anchor:      "\t\tif !a.Equal(b) {\n\t\t\treturn a.After(b)\n\t\t}",
			Replacement: "\t\tif !a.Equal(b) {\n\t\t\treturn a.Before(b)\n\t\t}",
			Test:        "TestDimension_TheLeastRecentIsDropped",
			Fails:       "survived although it was among the least recent",
			Guarantee:   "that eviction drops the least recently seen, so the view converges on what is happening now rather than on whoever arrived first",
		},
		{
			Name: "pressure-ties-are-broken", Package: "./core/pressure/",
			File:        "core/pressure/dimension.go",
			Anchor:      "\t\treturn subs[i] < subs[j]\n\t})",
			Replacement: "\t\treturn false\n\t})",
			Test:        "TestDimension_TiesAreBrokenLexicographicallyAndRepeatably",
			Fails:       "must not depend on map iteration",
			Guarantee:   "that subjects sharing a timestamp survive and order deterministically, since a tick stamps many at once and map order is randomised",
		},
		{
			Name: "pressure-the-remainder-is-rendered", Package: "./core/pressure/",
			File:        "core/pressure/dimension.go",
			Anchor:      "\tmore := d.omitted + (d.Len() - len(subs))",
			Replacement: "\tmore := d.Len() - len(subs)",
			Test:        "TestDimension_TheSummaryNamesWhatItLeftOut",
			Fails:       "does not account for every subject that arrived",
			Guarantee:   "that a truncated list says how many it left out, because sixteen shown of five hundred reads as sixteen",
		},
		{
			Name: "pressure-stale-subjects-expire", Package: "./core/pressure/",
			File:        "core/pressure/dimension.go",
			Anchor:      "\t\tif now.Sub(at) > ttl {\n\t\t\tdelete(d.seen, s)\n\t\t}",
			Replacement: "\t\t_ = at\n\t\t_ = s",
			Test:        "TestDimension_StaleSubjectsAreDroppedOnTheTickThatReads",
			Fails:       "survived a full expiry sweep",
			Guarantee:   "that a subject nobody has seen since its window drops out, so the view is about now",
		},
		{
			Name: "pressure-unconfigured-caps-emit-nothing", Package: "./core/pressure/",
			File:        "core/pressure/readings.go",
			Anchor:      "\tif c.SessionCap > 0 {",
			Replacement: "\tif true {",
			Test:        "TestReadings_UnconfiguredCapsProduceNoRows",
			Fails:       "was emitted with every cap unconfigured",
			Guarantee:   "that no limit configured produces no row, because a signal that can never raise is one more thing an operator learns to skip",
		},
		{
			Name: "pressure-a-throttled-source-is-credential", Package: "./core/pressure/",
			File:        "core/pressure/readings.go",
			Anchor:      "Signal: Signal{ID: ID{Name: SourcesThrottled, Subject: src},\n\t\t\t\tClass: Credential, Kind: Count},",
			Replacement: "Signal: Signal{ID: ID{Name: SourcesThrottled, Subject: src},\n\t\t\t\tClass: Capacity, Kind: Count},",
			Test:        "TestReadings_AThrottledSourceIsCredentialAndThenGoes",
			Fails:       "the throttled source raised as capacity",
			Guarantee:   "that the class is carried from the reading rather than assumed, so operations never repeats the conflation admission was corrected to remove",
		},
		{
			Name: "pressure-every-refusal-is-counted", Package: "./frontdoor/",
			File:        "frontdoor/listener.go",
			Anchor:      "\tif derr := l.denyWithOccurrence(stream, occ); derr != nil {",
			Replacement: "\tif derr := sendDenialOccurrence(stream, occ); derr != nil {",
			Test:        "TestPressureTick_NoRefusalBypassesTheCounter",
			Fails:       "refuse a client without counting it",
			Guarantee:   "that no branch can refuse a client without the refusal being counted, because a count kept beside the send is one somebody forgets on the next branch and the signal then under-reports silently",
		},
		{
			Name: "pressure-only-capacity-refusals-count", Package: "./frontdoor/",
			File: "frontdoor/pressure_tick.go",
			// REPOINTED WHEN THE EARLY RETURN LEFT. The anchor used to be the
			// guard itself -- `if occ.Charge != outcome.Capacity { return }` --
			// which turned out to be doing two jobs: keeping credential
			// refusals out of the rate, which is right, and keeping them out of
			// the breakdown entirely, which was the defect. Removing the guard
			// is no longer available as a mutation, so the break is now the
			// opposite one: count every class in the rate.
			Anchor:      "\tif occ.Charge == outcome.Capacity {\n\t\tm.denials.Add(now)\n\t}",
			Replacement: "\tm.denials.Add(now)",
			Test:        "TestPressureTick_OnlyCapacityRefusalsAreCounted",
			Fails:       "a capacity rate must not count",
			Guarantee:   "that a credential refusal never enters the capacity rate, which would put password guessing into a signal an operator answers by resizing a pool",
		},
		{
			Name: "pressure-observability-never-withholds", Package: "./frontdoor/",
			File:        "frontdoor/pressure_tick.go",
			Anchor:      "\tif err := sendDenialOccurrence(w, occ); err != nil {",
			Replacement: "\tif l.meter == nil {\n\t\treturn nil\n\t}\n\tif err := sendDenialOccurrence(w, occ); err != nil {",
			Test:        "TestPressureTick_NothingObservingStillRefuses",
			Fails:       "the client was sent nothing",
			Guarantee:   "that a client is answered whether or not anything is observing, because observability must never be able to withhold an answer",
		},
		{
			Name: "pressure-the-journal-keeps-the-class", Package: "./frontdoor/",
			File:        "frontdoor/pressure_loop.go",
			Anchor:      "Class: ev.Class.String(), Kind: ev.Kind.String(),",
			Replacement: "Class: \"capacity\", Kind: ev.Kind.String(),",
			Test:        "TestPressureLoop_AThrottledSourceReachesTheJournalAsCredential",
			Fails:       "reached the journal as",
			Guarantee:   "that the class rides all the way to the journal, so an operator grepping for capacity pressure does not find somebody guessing passwords",
		},
		{
			Name: "pressure-the-journal-keeps-the-figures", Package: "./frontdoor/",
			File:        "frontdoor/pressure_loop.go",
			Anchor:      "Value: ev.Value, Threshold: ev.Threshold, State: state,",
			Replacement: "State: state,",
			Test:        "TestPressureLoop_TheIncidentReachesTheJournal",
			Fails:       "the figures must travel with the crossing",
			Guarantee:   "that the value and the threshold travel with the crossing, because by the time anybody reads the journal the state that produced it has moved on",
		},
		{
			Name: "pressure-clears-reach-the-journal", Package: "./frontdoor/",
			File:        "frontdoor/pressure_loop.go",
			Anchor:      "\t\tstate := \"cleared\"\n\t\tif ev.Entered {\n\t\t\tstate = \"entered\"\n\t\t}",
			Replacement: "\t\tstate := \"cleared\"\n\t\tif !ev.Entered {\n\t\t\tcontinue\n\t\t}\n\t\tstate = \"entered\"",
			Test:        "TestPressureLoop_TheClearIsEmittedToo",
			Fails:       "want exactly one clear",
			Guarantee:   "that a clear is reported where its raise was, because an alert with no matching clear is the one people are trained to ignore",
		},
		{
			Name: "pressure-the-tick-is-waited-for", Package: "./frontdoor/",
			File:        "frontdoor/pressure_loop.go",
			Anchor:      "\tl.wg.Add(1)\n\tl.acceptMu.Unlock()",
			Replacement: "	l.acceptMu.Unlock()",
			Test:        "TestPressureLoop_CloseWaitsForTheTick",
			Fails:       "Close finished while the tick was still inside the reader",
			Guarantee:   "that closing the front door waits for the pressure tick, so a restarting host does not accumulate one ticker per restart reporting on instances that have stopped serving",
		},
		{
			Name: "pressure-the-wrapper-is-what-counts", Package: "./frontdoor/",
			File:        "frontdoor/pressure_tick.go",
			Anchor:      "\tif l.meter != nil {\n\t\tl.meter.recordDenial(occ)\n\t}\n",
			Replacement: "",
			Test:        "TestPressureTick_TheWrapperIsWhatCounts",
			Fails:       "the wrapper is the one place a refusal is counted",
			Guarantee:   "that the wrapper every refusal is routed through actually counts, since a structural guard proving the route and a charge guard driving the meter directly both survive a wrapper that only sends",
		},
		{
			Name: "pressure-only-a-refusal-that-landed-counts", Package: "./frontdoor/",
			File:        "frontdoor/pressure_tick.go",
			Anchor:      "\tif err := sendDenialOccurrence(w, occ); err != nil {\n\t\treturn err\n\t}\n\tif l.meter != nil {\n\t\tl.meter.recordDenial(occ)\n\t}\n\treturn nil",
			Replacement: "\tif l.meter != nil {\n\t\tl.meter.recordDenial(occ)\n\t}\n\treturn sendDenialOccurrence(w, occ)",
			Test:        "TestPressureTick_AFailedWriteIsNotCountedAsARefusal",
			Fails:       "refusals that never reached anybody raised",
			Guarantee:   "that a refusal which never reached its client is not counted, so a peer that has already gone cannot raise capacity pressure nobody was refused by",
		},
		{
			Name: "pressure-a-denial-lasts-a-full-window", Package: "./core/pressure/",
			File:        "core/pressure/window.go",
			Anchor:      "\tbuckets = 7",
			Replacement: "\tbuckets = 6",
			Test:        "TestWindow_ADenialLastsAtLeastAFullWindowWhereverItLands",
			Fails:       "has already been dropped",
			Guarantee:   "that a denial is retained for at least the whole window wherever in a bucket it lands, because under-retention makes the rate read low exactly during the burst the threshold exists for",
		},
		{
			Name: "pressure-a-closing-tick-does-not-dispatch", Package: "./frontdoor/",
			File:        "frontdoor/pressure_loop.go",
			Anchor:      "\t\t\tselect {\n\t\t\tcase <-l.closed:\n\t\t\t\treturn\n\t\t\tdefault:\n\t\t\t}\n\t\t\tl.emitPressure(caps)",
			Replacement: "\t\t\tl.emitPressure(caps)",
			Test:        "TestPressureLoop_ATickArrivingAtShutdownDoesNotDispatch",
			Fails:       "after the listener was closed",
			Guarantee:   "that no pressure dispatch starts once the close signal is visible, since a select with both cases ready may pick the ticker",
		},
		{
			Name: "pressure-the-breakdown-keeps-the-class", Package: "./core/pressure/",
			File:        "core/pressure/breakdown.go",
			Anchor:      "func (b *DenialBreakdown) Add(k DenialKey, now time.Time) {",
			Replacement: "func (b *DenialBreakdown) Add(k DenialKey, now time.Time) {\n\tk.Class = Capacity",
			Test:        "TestBreakdown_OneReasonUnderTwoClassesDoesNotMerge",
			Fails:       "rows for one reason under two classes",
			Guarantee:   "that one reason under two classes stays two rows, since a row labelled capacity about a credential refusal sends an operator in the opposite direction",
		},
		{
			Name: "pressure-the-breakdown-ages-out", Package: "./core/pressure/",
			File:        "core/pressure/breakdown.go",
			Anchor:      "\t\tif n := w.total(now); n > 0 {",
			Replacement: "\t\tif n := len(w.counts); n > 0 {",
			Test:        "TestBreakdown_RowsLeaveWhenTheWindowPasses",
			Fails:       "the breakdown still shows",
			Guarantee:   "that a refusal which stopped happening leaves the view, so the surface is about now rather than about a minute ago",
		},
		{
			Name: "pressure-the-breakdown-is-bounded", Package: "./core/pressure/",
			File:        "core/pressure/breakdown.go",
			Anchor:      "\tif len(b.windows) <= MaxSubjects {\n\t\treturn\n\t}",
			Replacement: "\tif true {\n\t\treturn\n\t}",
			Test:        "TestBreakdown_ItStaysBoundedAndSaysWhatItDropped",
			Fails:       "rows after 200 distinct reasons",
			Guarantee:   "that a map keyed by a reason arriving at runtime stays bounded, since one producer deriving a reason from anything a peer controls makes it a memory exhaustion primitive",
		},
		{
			Name: "pressure-the-view-reads-the-tick-latch", Package: "./core/pressure/",
			File:        "core/pressure/snapshot.go",
			Anchor:      "\t\tRaised: t.Raised(ID{Name: name, Subject: subject}),",
			Replacement: "\t\tRaised: capacity > 0 && value*100 >= capacity*enterPercent,",
			Test:        "TestSnapshot_RaisedComesFromTheSameLatchAsTheEvents",
			Fails:       "deciding for itself",
			Guarantee:   "that the view reads the tick's latch rather than judging, so the surface cannot contradict the journal about the same figure at the same instant",
		},
		{
			Name: "pressure-per-user-rows-attribute", Package: "./core/pressure/",
			File:        "core/pressure/snapshot.go",
			Anchor:      "\t\ts.PerUser = append(s.PerUser, t.row(SessionsUser, itoa(user), n, in.Caps.PerUserCap))",
			Replacement: "\t\t_ = n\n\t\ts.PerUser = append(s.PerUser, t.row(SessionsUser, itoa(user), int(user), in.Caps.PerUserCap))",
			Test:        "TestSnapshot_PerUserRowsAttributeCorrectlyAndLeadWithTheHungriest",
			Fails:       "sends somebody to talk to the wrong team",
			Guarantee:   "that a per-user row carries that user's own figure, since whose app is hungry was the question nobody could answer during the incident",
		},
		{
			Name: "pressure-capped-sections-carry-their-remainder", Package: "./core/pressure/",
			File:        "core/pressure/snapshot.go",
			Anchor:      "\ts.PerUser, s.PerUserOmitted = rank(s.PerUser)",
			Replacement: "\ts.PerUser, _ = rank(s.PerUser)",
			Test:        "TestSnapshot_TruncatedSectionsCarryTheirRemainder",
			Fails:       "does not account for the 40 that arrived",
			Guarantee:   "that every capped section reports its true remainder, since sixteen rows with none tells somebody the problem is smaller than it is",
		},
		{
			Name: "pressure-an-unreadable-view-says-so", Package: "./tui/",
			File:        "tui/pressure.go",
			Anchor:      "\t\treturn []pressureRow{{label: \"unavailable\", value: err.Error()}}",
			Replacement: "\t\treturn nil",
			Test:        "TestPressureView_AnUnreadableViewNamesItselfInsteadOfLookingCalm",
			Fails:       "it must name what went wrong",
			Guarantee:   "that a view which could not read says so, since an empty pressure table and a front door under no pressure render identically",
		},
		{
			Name: "pressure-the-pane-shows-the-class", Package: "./tui/",
			File:        "tui/pressure.go",
			Anchor:      "add(\"  \"+d.Reason, fmt.Sprintf(\"%d  (%s)\", d.Count, d.Class), d.Class == pressure.Capacity)",
			Replacement: "add(\"  \"+d.Reason, fmt.Sprintf(\"%d\", d.Count), d.Class == pressure.Capacity)",
			Test:        "TestPressureView_EveryRefusalShowsWhetherItIsUsOrThem",
			Fails:       "without naming both classes",
			Guarantee:   "that every refusal is rendered with its class, since a bare count sends somebody to resize a pool over password guessing or the reverse",
		},
		{
			Name: "pressure-no-limit-is-not-a-breach", Package: "./tui/",
			File:        "tui/pressure.go",
			Anchor:      "\tif r.Cap <= 0 {",
			Replacement: "\tif false {",
			Test:        "TestPressureView_NoLimitIsNotShownAsZero",
			Fails:       "reads as a breach",
			Guarantee:   "that an unconfigured cap renders as no limit rather than as a figure over zero, which invites reading an unlimited dimension as a breach",
		},
		{
			Name: "pressure-an-unknown-class-is-not-capacity", Package: "./tui/",
			File:        "tui/pressure.go",
			Anchor:      "\tif s == pressure.Capacity.String() {\n\t\treturn pressure.Capacity\n\t}\n\treturn pressure.Credential",
			Replacement: "\tif s == pressure.Credential.String() {\n\t\treturn pressure.Credential\n\t}\n\treturn pressure.Capacity",
			Test:        "TestPressureDecode_AnUnknownClassIsNotReadAsCapacity",
			Fails:       "sends somebody to change production capacity",
			Guarantee:   "that a class this build does not recognise is read as credential, because guessing wrong toward capacity costs a change to production capacity over nothing",
		},
		{
			Name: "pressure-the-decode-keeps-the-remainder", Package: "./tui/",
			File:        "tui/pressure.go",
			Anchor:      "\t\tPerUserOmitted:   intAt(m, \"per_user_omitted\"),",
			Replacement: "\t\tPerUserOmitted:   0,",
			Test:        "TestPressureDecode_TheViewSurvivesTheWire",
			Fails:       "loses its remainder",
			Guarantee:   "that the remainder survives the wire, since a list arriving without it tells the operator the problem is smaller than it is",
		},
		{
			Name: "pressure-the-view-is-admin-only", Package: "./rpc/",
			File:        "rpc/methods.go",
			Anchor:      "\t\tif _, err := s.auth.RequireAdminToken(ctx, token); err != nil {",
			Replacement: "\t\tif _, err := s.auth.ValidateToken(ctx, token); err != nil {",
			Test:        "TestPressure_TheViewIsAdminOnly",
			Fails:       "answered a non-admin",
			Guarantee:   "that the view is not disclosed to every authenticated caller, since it reports other users' session counts and the source addresses being refused",
		},
		{
			Name: "pressure-the-tunnel-recipe-matches-the-build", Package: "./cmd/autodb/",
			File:        "cmd/autodb/main.go",
			Anchor:      "const defaultWebPort = 7010",
			Replacement: "const defaultWebPort = 9999",
			Test:        "TestPressureDoc_TheTunnelRecipeMatchesTheBuild",
			Fails:       "the recipe forwards to remote port",
			Guarantee:   "that the documented tunnel names the port this build actually serves, since a recipe pasted under pressure that fails teaches somebody the surface does not work",
		},
		{
			Name: "pressure-the-daemon-is-given-something-to-observe", Package: "./cmd/autodb/",
			File:        "cmd/autodb/main.go",
			Anchor:      "\t\tCapacity:          eng,\n",
			Replacement: "",
			Test:        "TestFrontDoorWiring_TheListenerIsGivenSomethingToObserve",
			Fails:       "builds no meter",
			Guarantee:   "that a configured daemon actually builds a meter, since without one no refusal is counted, no crossing is emitted, no audit row is written, and the whole surface is correct and dead",
		},
		{
			Name: "pressure-credential-refusals-reach-the-view", Package: "./frontdoor/",
			File:        "frontdoor/pressure_tick.go",
			Anchor:      "\tm.breakdown.Add(pressure.DenialKey{Reason: string(occ.Reason), Class: pressureClass(occ.Charge)}, now)",
			Replacement: "\tif occ.Charge != outcome.Capacity {\n\t\tm.mu.Unlock()\n\t\treturn\n\t}\n\tm.breakdown.Add(pressure.DenialKey{Reason: string(occ.Reason), Class: pressureClass(occ.Charge)}, now)",
			Test:        "TestPressureTick_CredentialRefusalsRenderWithoutRaisingTheCapacityRate",
			Fails:       "left no row in the breakdown",
			Guarantee:   "that a credential refusal reaches the view at all, since the class column is the one thing distinguishing a full pool from somebody guessing passwords -- the distinction the incident got wrong",
		},
		{
			Name: "pressure-the-snapshot-holds-its-lock", Package: "./frontdoor/",
			File:        "frontdoor/pressure_loop.go",
			Anchor:      "\tl.meter.mu.Lock()\n\tdefer l.meter.mu.Unlock()\n\tdenials := l.meter.breakdown.Rows(now)\n\tomitted := l.meter.breakdown.Omitted()",
			Replacement: "\tl.meter.mu.Lock()\n\tdenials := l.meter.breakdown.Rows(now)\n\tomitted := l.meter.breakdown.Omitted()\n\tl.meter.mu.Unlock()",
			Test:        "TestPressureSnapshot_AReaderAndTheTickOverlap",
			// THE ONLY CONTROL HERE WHOSE BREAK HAS NO ASSERTION TO FAIL. A
			// lock released one line early returns the same snapshot and the
			// same error; the detector is the whole witness, so this control
			// scores nothing without -race.
			Race:      true,
			Timeout:   90 * time.Second,
			Fails:     "race detected during execution of test",
			Guarantee: "that the view is assembled under the lock the tick writes the latch beneath, since an operator opens this surface precisely when the tick has the most to write",
		},
		{
			Name: "pressure-the-protocol-bump-is-not-optional", Package: "./rpc/",
			File:        "rpc/server.go",
			Anchor:      "const Protocol int64 = 6",
			Replacement: "const Protocol int64 = 5",
			Test:        "TestProtocol_TheVerbSurfaceIsPinned",
			Fails:       "does not record",
			Guarantee:   "that a verb cannot be added on an unchanged protocol number, which is exactly what happened to sys.pressure while a cell pinning the number to 5 stayed green",
		},
		{
			Name: "pressure-a-new-verb-cannot-be-silent", Package: "./rpc/",
			File:        "rpc/methods.go",
			Anchor:      "func (s *Server) registerPressure() {\n",
			Replacement: "func (s *Server) registerPressure() {\n\ts.handle(\"sys.unrecorded\", func(ctx context.Context, req *golibrpc.Request) (any, error) { return nil, nil })\n",
			Test:        "TestProtocol_TheVerbSurfaceIsPinned",
			Fails:       "does not record",
			Guarantee:   "that adding a verb is a visible diff rather than one registration line among sixty-two, since the handshake is the only thing that can tell a newer frontend it has reached an older daemon",
		},
		{
			Name: "pressure-the-surface-stays-off-routable-interfaces", Package: "./cmd/autodb/",
			File:        "webserver/gateway.go",
			Anchor:      "func ListenAddr(port int) string { return fmt.Sprintf(\"127.0.0.1:%d\", port) }",
			Replacement: "func ListenAddr(port int) string { return fmt.Sprintf(\"0.0.0.0:%d\", port) }",
			Test:        "TestPressureTunnel_TheSurfaceIsUnreachableOffLoopback",
			Fails:       "a routable address",
			Guarantee:   "that the browser surface is unreachable without the forward, since it reports other people's session counts and the addresses being refused, and a widened bind would leave every tunnel cell passing",
		},
		{
			Name: "pressure-the-forward-reaches-the-real-address", Package: "./cmd/autodb/",
			File:        "webserver/gateway.go",
			Anchor:      "func ListenAddr(port int) string { return fmt.Sprintf(\"127.0.0.1:%d\", port) }",
			Replacement: "func ListenAddr(port int) string { return fmt.Sprintf(\"127.0.0.1:%d\", port+1) }",
			Test:        "TestPressureTunnel_TheDocumentedForwardReachesTheSurface",
			Timeout:     90 * time.Second,
			Fails:       "nothing answered through the forward",
			Guarantee:   "that the documented forward arrives at the address the gateway actually computes, rather than at a number two documents happen to agree on",
		},
		{
			Name: "pressure-the-shipped-frontend-agrees", Package: "./rpc/",
			File:        "lua/autodb/client.lua",
			Anchor:      "M.PROTOCOL = 6",
			Replacement: "M.PROTOCOL = 5",
			Test:        "TestProtocol_TheShippedFrontendSpeaksTheSameNumber",
			Fails:       "refuse each other",
			Guarantee:   "that the plugin and the daemon built from one commit speak one number, since a mismatch between them produces the message a STALE pairing gives and sends the user to refresh a binary that is already correct",
		},
		{
			Name: "card-a-ceiling-shows-its-figure", Package: "./tui/",
			File:        "tui/conncard.go",
			Anchor:      "\treturn fmt.Sprintf(\"%d\", n)",
			Replacement: "\treturn \"\"",
			Test:        "TestCardBudget_TheCeilingsCarryTheirScope",
			Fails:       "does not carry its figure",
			Guarantee:   "that each ceiling is shown beside the scope it belongs to, since a scope with no number and a number with no scope are each half of the sentence a developer needs",
		},
		{
			Name: "card-an-unreported-ceiling-is-not-zero", Package: "./tui/",
			File:        "tui/conncard.go",
			Anchor:      "\tif n <= 0 {\n\t\treturn \"not reported\"\n\t}\n",
			Replacement: "",
			Test:        "TestCardBudget_AnUnreportedCeilingSaysSoRatherThanSayingZero",
			Fails:       "an unreported ceiling renders as",
			Guarantee:   "that a figure an older daemon did not send reads as unreported rather than as a cap of zero, which would tell a developer they may open no sessions at all",
		},
		{
			Name: "card-does-no-arithmetic", Package: "./tui/",
			File:        "tui/conncard.go",
			Anchor:      "\tp(\"  These are ceilings, and they are SHARED. None of them is yours alone.\")",
			Replacement: "\tp(\"  Your demand is  processes x databases x connections-per-pool.\")",
			Test:        "TestCardBudget_ItDoesNoArithmetic",
			Fails:       "processes x databases",
			Guarantee:   "that the card asks nobody to compute a pool size, since each connection already carries the bound autodb holds a client to",
		},
		{
			Name: "card-advertises-no-per-source-cap", Package: "./tui/",
			File:        "tui/conncard.go",
			Anchor:      "\tp(\"  If you are being refused and want to know why, open the pressure view\")",
			Replacement: "\tp(\"  %-22s %-9s %s\", \"connections per source\", \"16\", \"everyone behind your address\")\n\tp(\"  If you are being refused and want to know why, open the pressure view\")",
			Test:        "TestCardBudget_NoPerSourceConcurrencyCapIsAdvertised",
			Fails:       "advertises a per-source limit",
			Guarantee:   "that the failure-rate throttle is never displayed as a concurrency ceiling, which would teach the same capacity-for-credential confusion the incident was made of",
		},
		{
			Name: "card-tells-nobody-to-configure-a-pool", Package: "./tui/",
			File:        "tui/conncard.go",
			Anchor:      "\tp(\"  client is configured for them.\")",
			Replacement: "\tp(\"  client is configured for them. Set PG_MAX_OPEN_CONNS to match.\")",
			Test:        "TestCardBudget_ItTellsNobodyToConfigureAPool",
			Fails:       "tells somebody to set",
			Guarantee:   "that the card hands out no pool-sizing recipe, since a developer points an application at the front door and it works -- advice here teaches the thing the scheduler exists to stop them needing",
		},
		{
			Name: "card-shows-no-live-availability", Package: "./tui/",
			File:        "tui/conncard.go",
			Anchor:      "\tp(\"  (System -> Pressure). It carries a timestamp; this card does not.\")",
			Replacement: "\tp(\"  %d backend connections are free right now.\", ep.MaxTargetConns)",
			Test:        "TestCardBudget_ItShowsNoLiveAvailability",
			Fails:       "reads as a live figure",
			Guarantee:   "that nothing on a card shown once and never recoverable claims to describe this instant, since such a figure is stale before it is read",
		},
		{
			Name: "card-an-unusable-token-gets-no-advice", Package: "./tui/",
			File:        "tui/conncard.go",
			Anchor:      "\tif !ep.Configured() {\n\t\treturn\n\t}\n\n\tp(\"LIMITS THAT APPLY TO THIS TOKEN\")",
			Replacement: "\tp(\"LIMITS THAT APPLY TO THIS TOKEN\")",
			Test:        "TestCardBudget_AnUnusableTokenGetsNoBudgetBlock",
			Fails:       "still got pool-sizing advice",
			Guarantee:   "that a token which cannot be used anywhere is not buried under pool sizing, between the reader and the one warning they can act on",
		},
		{
			Name: "card-the-connections-own-bound-leads", Package: "./tui/",
			File:        "tui/conncard.go",
			Anchor:      "\tif conn.PoolMaxConns > 0 {",
			Replacement: "\tif false && conn.PoolMaxConns > 0 {",
			Test:        "TestCardBudget_TheConnectionsOwnBoundLeads",
			Fails:       "no line on the card contains",
			Guarantee:   "that the bound actually governing this token is the one shown, since it is the number autodb holds a client to whether or not the client sized itself",
			Name:        "r7-the-clock-is-not-a-wire-timer", Package: "./core/exec/",
			File:        "core/exec/wire_extended_objects.go",
			Anchor:      "func (o *extObjects) queueWire() { o.segment = append(o.segment, segStep{}) }",
			Replacement: "func (o *extObjects) queueWire() {\n\to.noteProgress()\n\to.segment = append(o.segment, segStep{})\n}",
			Test:        "TestR7_OrdinaryTrafficDoesNotAdvanceTheDependencyClock",
			Fails:       "the dependency clock advanced",
			Guarantee:   "that ordinary wire traffic does not reset the dependency clock, since a client which keeps the wire busy and never uses what it prepared is the one case R7 exists to catch",
		},
		{
			Name: "r7-using-an-object-is-progress", Package: "./core/exec/",
			File:        "core/exec/wire_extended_objects.go",
			Anchor:      "func (o *extObjects) queueExec() {\n\to.noteProgress()",
			Replacement: "func (o *extObjects) queueExec() {",
			Test:        "TestR7_TouchingTheObjectsResetsTheClock",
			Fails:       "would be reclaimed as though it had abandoned them",
			Guarantee:   "that executing a portal counts as dependency progress, so a session genuinely using its objects is never reclaimed as though it had abandoned them",
		},
		{
			Name: "r7-a-pending-close-is-still-held", Package: "./core/exec/",
			File:        "core/exec/wire_extended_objects.go",
			Anchor:      "\treturn len(o.statements) > 0 || len(o.portals) > 0 || len(o.pendingCloses) > 0",
			Replacement: "\treturn len(o.statements) > 0 || len(o.portals) > 0",
			Test:        "TestR7_AnEmptyStoreIsNotACandidate",
			Fails:       "reports holding nothing",
			Guarantee:   "that a store is counted as non-empty while anything is held, since the target may still hold an object whose name is already free",
		},
		{
			Name: "r7-the-rung-fires", Package: "./core/exec/",
			File:        "core/exec/session_engine.go",
			Anchor:      "\treturn s.ext.dependencyIdleFor(now) >= r7DependencyBound",
			Replacement: "\treturn false",
			Test:        "TestR7_AStalledDependencyOnALiveWireIsReclaimed",
			Fails:       "was not reclaimed",
			Guarantee:   "that a backend pinned by an object nobody has touched is reclaimed, since no other rung ever reaches a session whose wire is still active",
		},
		{
			Name: "r7-it-spares-work-in-flight", Package: "./core/exec/",
			File:        "core/exec/session_engine.go",
			Anchor:      "\tif s.busy || s.tx != nil || s.ext == nil || !s.ext.holdsAnything() {",
			Replacement: "\tif s.ext == nil || !s.ext.holdsAnything() {",
			Test:        "TestR7_ItSparesEverySessionThatWouldBeHarmed",
			Fails:       "the rung claimed a session where",
			Guarantee:   "that the rung never takes a session with a request in flight or a transaction open, since ending either destroys work nobody abandoned",
		},
		{
			Name: "r7-the-bound-is-the-promise", Package: "./core/exec/",
			File:        "core/exec/session_engine.go",
			Anchor:      ">= r7DependencyBound\n}",
			Replacement: ">= r7DependencyBound/2\n}",
			Test:        "TestR7_ADependencyInsideItsBoundIsNotReclaimed",
			Fails:       "was reclaimed",
			Guarantee:   "that a dependency inside its bound is left alone, because the bound is the promise made to everybody legitimately holding an object across a quiet period",
		},
	}
}
