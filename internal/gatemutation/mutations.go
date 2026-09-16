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
			Anchor:      "\t\treserved := eligible && s.beginCloseLocked(\"\", ReasonDemandReclaimed)",
			Replacement: "\t\ts.mu.Unlock()\n\t\ts.mu.Lock()\n\t\treserved := eligible && s.beginCloseLocked(\"\", ReasonDemandReclaimed)",
			Test:        "TestDemandReclaim_NothingCanSlipBetweenJudgingAndClaiming",
			Fails:       "although a statement started between the check and the claim",
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
	}
}
