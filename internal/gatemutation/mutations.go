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
	// Test is the cell that must FAIL once the mutation is applied. It must
	// exist; a control naming an absent test proves nothing and has happened.
	Test string
	// Guarantee says, in one sentence, what goes unproven if this survives.
	// It is the sentence a reviewer reads when a control comes back green.
	Guarantee string
}

// All returns every control, in ledger order.
//
// ORDERED AND NAMED rather than discovered, because a ledger has to be
// comparable between runs: "control 6 survived" must mean the same thing in
// two ledgers a week apart.
func All() []Mutation {
	return []Mutation{
		{
			Name: "serve-line-at-enqueue", File: "core/exec/scheduler.go",
			Anchor:      "\tr.serveLine()\n\tif w.state == waitResolved {",
			Replacement: "\tif w.state == waitResolved {",
			Test:        "TestScheduler_AnEligibleNewcomerIsServedAtEnqueueTime",
			Guarantee: "that a request whose own target is free is served when it arrives, " +
				"rather than waiting for an unrelated release that may never come",
		},
		{
			Name: "transaction-bound-is-a-deadline", File: "core/exec/session.go",
			Anchor:      "\t\tif now.Before(until) {",
			Replacement: "\t\tif true || now.Before(until) {",
			Test:        "TestScheduler_AnExpiredTransactionIsNotAReasonToRefuse",
			Guarantee: "that a transaction past its bound counts as capacity that IS coming, " +
				"so a request is not told nothing is coming moments before it arrives",
		},
		{
			Name: "cancellation-undoes-its-admission", File: "core/exec/scheduler.go",
			Anchor:      "\tif err := <-w.done; err == nil {\n\t\tr.remove(w.s)\n\t}",
			Replacement: "\t_ = w",
			Test:        "TestScheduler_ACancellationThatLosesToAGrantUndoesTheAdmission",
			Guarantee: "that an admission granted in the instant a caller gives up is handed " +
				"back, rather than leaking a session nothing is coming to close",
		},
		{
			Name: "line-skips-the-ineligible", File: "core/exec/scheduler.go",
			Anchor:      "\t\t\t\tw.blockedBy = err\n\t\t\t\tcontinue",
			Replacement: "\t\t\t\tw.blockedBy = err\n\t\t\t\tbreak",
			Test:        "TestScheduler_AFullTargetDoesNotBlockTheRestOfTheLine",
			Guarantee: "that one saturated target cannot hold up every request for every " +
				"other target",
		},
		{
			Name: "timeout-unwraps-alone", File: "core/exec/scheduler.go",
			Anchor:      "func (e *QueueTimeoutError) Unwrap() error { return ErrQueueTimeout }",
			Replacement: "func (e *QueueTimeoutError) Unwrap() []error { return []error{ErrQueueTimeout, e.blockedBy} }",
			Test:        "TestAdmissionWait_TheBlockingCapIsDiagnosisAndNotAnIdentity",
			Guarantee: "that the blocking cap stays diagnosis rather than becoming an identity " +
				"an ordered switch can select, which would make the queue-timeout answer " +
				"unreachable in production while still registered",
		},
		{
			Name: "server-wait-uses-its-seam", File: "core/exec/scheduler.go",
			Anchor:      "\t\ttimer := r.serverTimer(queueWait)",
			Replacement: "\t\ttimer := time.NewTimer(queueWait)",
			Test:        "TestScheduler_TheServerWaitExpiresWithItsOwnIdentity",
			Guarantee: "that the server's wait is armed through the injectable seam, without " +
				"which a gate silently becomes a multi-minute one and eventually reads as a hang",
		},
		{
			Name: "the-caller-owns-an-exact-tie", File: "core/exec/scheduler.go",
			Anchor:      "\tif d, ok := ctx.Deadline(); !ok || d.After(serverDeadline) {",
			Replacement: "\tif d, ok := ctx.Deadline(); !ok || !d.Before(serverDeadline) {",
			Test:        "TestScheduler_OneBoundOwnsTheWaitDeterministically",
			Guarantee: "that which bound owns a wait is decided by policy rather than by which " +
				"channel the runtime happens to see first",
		},
		{
			Name: "release-serves-the-line", File: "core/exec/session.go",
			Anchor:      "\t\tr.serveLine()\n\t}\n\tr.mu.Unlock()",
			Replacement: "\t}\n\tr.mu.Unlock()",
			Test:        "TestScheduler_AReleaseNeverLeavesAnAdmittableWaiterWaiting",
			Guarantee: "that freed capacity never sits idle while somebody able to use it is " +
				"still queued, which is what makes joining the line safe rather than a disadvantage",
		},
		{
			Name: "expired-wait-names-its-blocker", File: "core/exec/wire_session.go",
			Anchor:      "\t\treturn denyAfterAuthorizationWithDetail(DenyQueueTimeout, detail)",
			Replacement: "\t\treturn denyAfterAuthorization(DenyQueueTimeout)",
			Test:        "TestOpenWireSession_AWaitThatExpiresIsRecordedAsAWaitNotAsACapRefusal",
			Guarantee: "that the one record of an expired wait says which limit to raise, not " +
				"merely that somebody waited",
		},
		{
			Name: "the-wait-arm-comes-first", File: "core/exec/wire_session.go",
			Anchor:      "\tcase errors.Is(rerr, ErrQueueTimeout):",
			Replacement: "\tcase errors.Is(rerr, ErrLeaseCapExceeded):\n\t\treturn denyAfterAuthorization(DenyLeaseCap)\n\tcase errors.Is(rerr, ErrQueueTimeout):",
			Test:        "TestAdmissionDenial_TheWaitOutranksTheCapItWaitedOn",
			Guarantee: "that a request which waited is never recorded as one refused on arrival, " +
				"independently of what the error happens to unwrap to",
		},
		{
			Name: "coordinates-come-from-the-code", File: "internal/gatematrix/coords.go",
			Anchor:      "\tlines := strings.Split(doc, \"\\n\")",
			Replacement: "\tif true {\n\t\treturn doc\n\t}\n\tlines := strings.Split(doc, \"\\n\")",
			Test:        "TestCoordinates_AMovedUseIsCorrected",
			Guarantee: "that the generator reads the tree rather than echoing back the document " +
				"it was handed",
		},
		{
			Name: "the-matrix-is-current", File: "docs/admission-gate-matrix.md",
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
			Name: "identity-excludes-git", File: "internal/gateidentity/identity.go",
			Anchor:      "\t\".git\": true, \"node_modules\": true, \".cache\": true,",
			Replacement: "\t\"node_modules\": true, \".cache\": true,",
			Test:        "TestIdentity_AWorktreeGitFileIsNotPartOfTheFingerprint",
			Guarantee: "that a worktree and its .git-excluded copy can be compared at all, " +
				"without which the identity gate rejects every honest copy and teaches " +
				"everyone to ignore it",
		},
	}
}
