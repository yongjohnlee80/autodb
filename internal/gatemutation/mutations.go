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
			// Detects the break by waiting out the real server bound.
			Timeout: 150 * time.Second,
			Guarantee: "that a request whose own target is free is served when it arrives, " +
				"rather than waiting for an unrelated release that may never come",
		},
		{
			Name: "transaction-bound-is-a-deadline", Package: "./core/exec/", File: "core/exec/session.go",
			Anchor:      "\t\tif now.Before(until) {",
			Replacement: "\t\tif true || now.Before(until) {",
			Test:        "TestScheduler_AnExpiredTransactionIsNotAReasonToRefuse",
			Guarantee: "that a transaction past its bound counts as capacity that IS coming, " +
				"so a request is not told nothing is coming moments before it arrives",
		},
		{
			Name: "cancellation-undoes-its-admission", Package: "./core/exec/", File: "core/exec/scheduler.go",
			Anchor:      "\tif err := <-w.done; err == nil {\n\t\tr.remove(w.s)\n\t}",
			Replacement: "\t_ = w",
			Test:        "TestScheduler_ACancellationThatLosesToAGrantUndoesTheAdmission",
			Count:       20,
			Guarantee: "that an admission granted in the instant a caller gives up is handed " +
				"back, rather than leaking a session nothing is coming to close",
		},
		{
			Name: "line-skips-the-ineligible", Package: "./core/exec/", File: "core/exec/scheduler.go",
			Anchor:      "\t\t\t\tw.blockedBy = err\n\t\t\t\tcontinue",
			Replacement: "\t\t\t\tw.blockedBy = err\n\t\t\t\tbreak",
			Test:        "TestScheduler_AFullTargetDoesNotBlockTheRestOfTheLine",
			Guarantee: "that one saturated target cannot hold up every request for every " +
				"other target",
		},
		{
			Name: "timeout-unwraps-alone", Package: "./core/exec/", File: "core/exec/scheduler.go",
			Anchor:      "func (e *QueueTimeoutError) Unwrap() error { return ErrQueueTimeout }",
			Replacement: "func (e *QueueTimeoutError) Unwrap() []error { return []error{ErrQueueTimeout, e.blockedBy} }",
			Test:        "TestAdmissionWait_TheBlockingCapIsDiagnosisAndNotAnIdentity",
			Guarantee: "that the blocking cap stays diagnosis rather than becoming an identity " +
				"an ordered switch can select, which would make the queue-timeout answer " +
				"unreachable in production while still registered",
		},
		{
			Name: "server-wait-uses-its-seam", Package: "./core/exec/", File: "core/exec/scheduler.go",
			Anchor:      "\t\ttimer := r.serverTimer(queueWait)",
			Replacement: "\t\ttimer := time.NewTimer(queueWait)",
			Test:        "TestScheduler_TheServerWaitExpiresWithItsOwnIdentity",
			Guarantee: "that the server's wait is armed through the injectable seam, without " +
				"which a gate silently becomes a multi-minute one and eventually reads as a hang",
		},
		{
			Name: "the-caller-owns-an-exact-tie", Package: "./core/exec/", File: "core/exec/scheduler.go",
			Anchor:      "\tif d, ok := ctx.Deadline(); !ok || d.After(serverDeadline) {",
			Replacement: "\tif d, ok := ctx.Deadline(); !ok || !d.Before(serverDeadline) {",
			Test:        "TestScheduler_OneBoundOwnsTheWaitDeterministically",
			Guarantee: "that which bound owns a wait is decided by policy rather than by which " +
				"channel the runtime happens to see first",
		},
		{
			Name: "release-serves-the-line", Package: "./core/exec/", File: "core/exec/session.go",
			Anchor:      "\t\tr.serveLine()\n\t}\n\tr.mu.Unlock()",
			Replacement: "\t}\n\tr.mu.Unlock()",
			Test:        "TestScheduler_AReleaseNeverLeavesAnAdmittableWaiterWaiting",
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
			Guarantee: "that the one record of an expired wait says which limit to raise, not " +
				"merely that somebody waited",
		},
		{
			Name: "the-wait-arm-comes-first", Package: "./core/exec/", File: "core/exec/wire_session.go",
			Anchor:      "\tcase errors.Is(rerr, ErrQueueTimeout):",
			Replacement: "\tcase errors.Is(rerr, ErrLeaseCapExceeded):\n\t\treturn denyAfterAuthorization(DenyLeaseCap)\n\tcase errors.Is(rerr, ErrQueueTimeout):",
			Test:        "TestAdmissionDenial_TheWaitOutranksTheCapItWaitedOn",
			Guarantee: "that a request which waited is never recorded as one refused on arrival, " +
				"independently of what the error happens to unwrap to",
		},
		{
			Name: "coordinates-come-from-the-code", Package: "./internal/gatematrix/", File: "internal/gatematrix/coords.go",
			Anchor:      "\tlines := strings.Split(doc, \"\\n\")",
			Replacement: "\tif true {\n\t\treturn doc\n\t}\n\tlines := strings.Split(doc, \"\\n\")",
			Test:        "TestCoordinates_AMovedUseIsCorrected",
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
			Guarantee: "that a coordinate drifting out of step with the code is caught rather " +
				"than discovered later by a downstream walk",
		},
		{
			Name: "identity-excludes-git", Package: "./internal/gateidentity/", File: "internal/gateidentity/identity.go",
			Anchor:      "\t\".git\": true, \"node_modules\": true, \".cache\": true,",
			Replacement: "\t\"node_modules\": true, \".cache\": true,",
			Test:        "TestIdentity_AWorktreeGitFileIsNotPartOfTheFingerprint",
			Guarantee: "that a worktree and its .git-excluded copy can be compared at all, " +
				"without which the identity gate rejects every honest copy and teaches " +
				"everyone to ignore it",
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
	}
}
