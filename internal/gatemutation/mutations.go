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
