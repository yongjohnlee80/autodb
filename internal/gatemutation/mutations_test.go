package gatemutation

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// repoRoot is two levels up from this package.
const repoRoot = "../.."

// EVERY CONTROL STILL POINTS AT THE CODE IT MEANS TO BREAK.
//
// THIS IS THE CELL THE PROCESS WAS MISSING. A control whose anchor no longer
// matches is applied to nothing, and a run can still report a verdict for it —
// so a guarantee stops being proven and the ledger says otherwise. Exactly once
// is the property: twice would mutate something the control never intended, and
// zero times would mutate nothing at all.
func TestMutations_EveryAnchorMatchesExactlyOnce(t *testing.T) {
	for _, m := range All() {
		t.Run(m.Name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(repoRoot, m.File))
			if err != nil {
				t.Fatalf("%s: %v", m.File, err)
			}
			switch n := strings.Count(string(body), m.Anchor); n {
			case 1: // the only acceptable answer
			case 0:
				t.Errorf("the anchor is gone from %s, so this control now breaks NOTHING and "+
					"a run would still score it. Unproven if it stays this way: %s",
					m.File, m.Guarantee)
			default:
				t.Errorf("the anchor matches %d times in %s, so applying it would change code "+
					"this control never meant to touch", n, m.File)
			}
		})
	}
}

// EVERY CONTROL NAMES A TEST THAT EXISTS.
//
// THIS CELL EXISTS BECAUSE THE OPPOSITE HAPPENED. A control named a cell that
// had been deleted by an editing mistake, so nothing could have caught the
// break it applied; the run scored it INVALID only because the runner thought
// to check, and no part of the process would otherwise have noticed that a
// guarantee had silently stopped being proven.
func TestMutations_EveryNamedTestExists(t *testing.T) {
	names := declaredTests(t)
	for _, m := range All() {
		// IN THE PACKAGE THE CONTROL NAMES, not merely somewhere in the tree.
		// A cell that exists in a different package is one the runner's
		// `-run` will never select, so the control would apply its break and
		// score a verdict against a test that never executed.
		if pkgs := names[m.Test]; len(pkgs) > 0 && !pkgs[m.Package] {
			where := make([]string, 0, len(pkgs))
			for p := range pkgs {
				where = append(where, p)
			}
			sort.Strings(where)
			t.Errorf("control %q names %s in package %s, but that test is declared in %v",
				m.Name, m.Test, m.Package, where)
		}
		if len(names[m.Test]) == 0 {
			t.Errorf("control %q names %s, which no test file declares. Nothing can catch the "+
				"break it applies, and what goes unproven is: %s", m.Name, m.Test, m.Guarantee)
		}
	}
}

// EVERY CONTROL IS DISTINCT AND SAYS WHAT IT PROTECTS.
//
// A duplicate name makes two ledgers incomparable; a missing guarantee makes a
// green verdict unreadable, because the reviewer cannot tell what survived.
func TestMutations_TheSetIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range All() {
		if seen[m.Name] {
			t.Errorf("two controls are both named %q, so a ledger cannot say which survived", m.Name)
		}
		seen[m.Name] = true

		if m.Anchor == "" || m.Replacement == m.Anchor {
			t.Errorf("control %q changes nothing", m.Name)
		}
		if !strings.HasPrefix(m.Test, "Test") {
			t.Errorf("control %q names %q, which is not a test", m.Name, m.Test)
		}
		// THE PACKAGE IS PART OF THE ADDRESS. A runner handed a bare test name
		// must guess which package to run, and a wrong guess runs nothing --
		// which is indistinguishable, in a ledger, from a control that ran and
		// failed to discriminate.
		if !strings.HasPrefix(m.Package, "./") || !strings.HasSuffix(m.Package, "/") {
			t.Errorf("control %q names package %q; want a go-test path like ./core/exec/",
				m.Name, m.Package)
		}
		if _, err := os.Stat(filepath.Join(repoRoot, strings.TrimPrefix(m.Package, "./"))); err != nil {
			t.Errorf("control %q names package %q, which does not exist: %v", m.Name, m.Package, err)
		}
		if m.Fails == "" {
			t.Errorf("control %q declares no failure fingerprint; RED would then mean only "+
				"that something in %s failed, which credits it for a neighbour's assertion",
				m.Name, m.Test)
		}
		if len(m.Guarantee) < 30 {
			t.Errorf("control %q does not say what goes unproven if it survives; a green "+
				"verdict would be unreadable", m.Name)
		}
		if strings.Contains(m.File, "..") || filepath.IsAbs(m.File) {
			t.Errorf("control %q names a path outside the repository: %s", m.Name, m.File)
		}
	}
	// THE SET IS PINNED BY NAME, NOT BY SIZE.
	//
	// A count let one guarantee be deleted and an unrelated control added in
	// the same change: the number stayed right, the guard stayed green, and a
	// guarantee stopped being proven with nothing saying so. Membership is the
	// property worth holding, so the expected names live below and a diff that
	// changes coverage has to edit them -- which is where a reviewer can see
	// what was lost.
	want := expectedControls()
	got := map[string][3]string{}
	for _, m := range All() {
		got[m.Name] = [3]string{m.File, m.Package, m.Test}
	}
	for name, fields := range want {
		have, ok := got[name]
		if !ok {
			t.Errorf("control %q is gone. Say which guarantee stopped being proven and why "+
				"that is acceptable, then remove it from expectedControls", name)
			continue
		}
		if have != fields {
			t.Errorf("control %q keeps its name but now attacks %v instead of %v; the set "+
				"looks unchanged while what it proves has moved", name, have, fields)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("control %q is new and not in expectedControls; add it there in the same "+
				"change, so coverage moves visibly", name)
		}
	}
}

// expectedControls is what this suite expects to exist, by name AND by the
// three fields that say what a control actually attacks.
//
// NAMES ALONE WERE NOT ENOUGH. An existing name could be quietly retargeted to
// a different file, package or cell while membership stayed green — the set
// would look unchanged while what it proved had moved somewhere else entirely.
// Edited deliberately, in the same change as the control it names, so coverage
// cannot move without somebody writing the move down.
func expectedControls() map[string][3]string {
	return map[string][3]string{
		// name: {File, Package, Test}
		"r7-the-clock-is-not-a-wire-timer":               {"core/exec/wire_extended_objects.go", "./core/exec/", "TestR7_OrdinaryTrafficDoesNotAdvanceTheDependencyClock"},
		"r7-using-an-object-is-progress":                 {"core/exec/wire_extended_objects.go", "./core/exec/", "TestR7_TouchingTheObjectsResetsTheClock"},
		"r7-a-pending-close-is-still-held":               {"core/exec/wire_extended_objects.go", "./core/exec/", "TestR7_AnEmptyStoreIsNotACandidate"},
		"r7-the-rung-fires":                              {"core/exec/session_engine.go", "./core/exec/", "TestR7_AStalledDependencyOnALiveWireIsReclaimed"},
		"r7-it-spares-work-in-flight":                    {"core/exec/session_engine.go", "./core/exec/", "TestR7_ItSparesEverySessionThatWouldBeHarmed"},
		"r7-the-bound-is-the-promise":                    {"core/exec/session_engine.go", "./core/exec/", "TestR7_ADependencyInsideItsBoundIsNotReclaimed"},
		"pressure-the-breakdown-keeps-the-class":         {"core/pressure/breakdown.go", "./core/pressure/", "TestBreakdown_OneReasonUnderTwoClassesDoesNotMerge"},
		"pressure-the-breakdown-ages-out":                {"core/pressure/breakdown.go", "./core/pressure/", "TestBreakdown_RowsLeaveWhenTheWindowPasses"},
		"pressure-the-breakdown-is-bounded":              {"core/pressure/breakdown.go", "./core/pressure/", "TestBreakdown_ItStaysBoundedAndSaysWhatItDropped"},
		"pressure-the-view-reads-the-tick-latch":         {"core/pressure/snapshot.go", "./core/pressure/", "TestSnapshot_RaisedComesFromTheSameLatchAsTheEvents"},
		"pressure-per-user-rows-attribute":               {"core/pressure/snapshot.go", "./core/pressure/", "TestSnapshot_PerUserRowsAttributeCorrectlyAndLeadWithTheHungriest"},
		"pressure-capped-sections-carry-their-remainder": {"core/pressure/snapshot.go", "./core/pressure/", "TestSnapshot_TruncatedSectionsCarryTheirRemainder"},
		"pressure-an-unreadable-view-says-so":            {"tui/pressure.go", "./tui/", "TestPressureView_AnUnreadableViewNamesItselfInsteadOfLookingCalm"},
		"pressure-the-pane-shows-the-class":              {"tui/pressure.go", "./tui/", "TestPressureView_EveryRefusalShowsWhetherItIsUsOrThem"},
		"pressure-no-limit-is-not-a-breach":              {"tui/pressure.go", "./tui/", "TestPressureView_NoLimitIsNotShownAsZero"},
		"pressure-an-unknown-class-is-not-capacity":      {"tui/pressure.go", "./tui/", "TestPressureDecode_AnUnknownClassIsNotReadAsCapacity"},
		"pressure-the-decode-keeps-the-remainder":        {"tui/pressure.go", "./tui/", "TestPressureDecode_TheViewSurvivesTheWire"},
		"pressure-the-view-is-admin-only":                {"rpc/methods.go", "./rpc/", "TestPressure_TheViewIsAdminOnly"},
		"pressure-the-tunnel-recipe-matches-the-build":   {"cmd/autodb/main.go", "./cmd/autodb/", "TestPressureDoc_TheTunnelRecipeMatchesTheBuild"},

		// Review round: the five production contracts the 84-control sweep did
		// not exercise. Four of them are the same shape -- a cell asserting an
		// output cannot see whether the mechanism inside it did any work.
		"pressure-the-daemon-is-given-something-to-observe":  {"cmd/autodb/main.go", "./cmd/autodb/", "TestFrontDoorWiring_TheListenerIsGivenSomethingToObserve"},
		"pressure-credential-refusals-reach-the-view":        {"frontdoor/pressure_tick.go", "./frontdoor/", "TestPressureTick_CredentialRefusalsRenderWithoutRaisingTheCapacityRate"},
		"pressure-the-snapshot-holds-its-lock":               {"frontdoor/pressure_loop.go", "./frontdoor/", "TestPressureSnapshot_AReaderAndTheTickOverlap"},
		"pressure-the-protocol-bump-is-not-optional":         {"rpc/server.go", "./rpc/", "TestProtocol_TheVerbSurfaceIsPinned"},
		"pressure-a-new-verb-cannot-be-silent":               {"rpc/methods.go", "./rpc/", "TestProtocol_TheVerbSurfaceIsPinned"},
		"pressure-the-surface-stays-off-routable-interfaces": {"webserver/gateway.go", "./cmd/autodb/", "TestPressureTunnel_TheSurfaceIsUnreachableOffLoopback"},
		"pressure-the-forward-reaches-the-real-address":      {"webserver/gateway.go", "./cmd/autodb/", "TestPressureTunnel_TheDocumentedForwardReachesTheSurface"},
		"pressure-the-shipped-frontend-agrees":               {"lua/autodb/client.lua", "./rpc/", "TestProtocol_TheShippedFrontendSpeaksTheSameNumber"},
		"pressure-only-a-refusal-that-landed-counts":         {"frontdoor/pressure_tick.go", "./frontdoor/", "TestPressureTick_AFailedWriteIsNotCountedAsARefusal"},
		"pressure-a-denial-lasts-a-full-window":              {"core/pressure/window.go", "./core/pressure/", "TestWindow_ADenialLastsAtLeastAFullWindowWhereverItLands"},
		"pressure-a-closing-tick-does-not-dispatch":          {"frontdoor/pressure_loop.go", "./frontdoor/", "TestPressureLoop_ATickArrivingAtShutdownDoesNotDispatch"},
		"pressure-the-wrapper-is-what-counts":                {"frontdoor/pressure_tick.go", "./frontdoor/", "TestPressureTick_TheWrapperIsWhatCounts"},
		"pressure-every-refusal-is-counted":                  {"frontdoor/listener.go", "./frontdoor/", "TestPressureTick_NoRefusalBypassesTheCounter"},
		"pressure-only-capacity-refusals-count":              {"frontdoor/pressure_tick.go", "./frontdoor/", "TestPressureTick_OnlyCapacityRefusalsAreCounted"},
		"pressure-observability-never-withholds":             {"frontdoor/pressure_tick.go", "./frontdoor/", "TestPressureTick_NothingObservingStillRefuses"},
		"pressure-the-journal-keeps-the-class":               {"frontdoor/pressure_loop.go", "./frontdoor/", "TestPressureLoop_AThrottledSourceReachesTheJournalAsCredential"},
		"pressure-the-journal-keeps-the-figures":             {"frontdoor/pressure_loop.go", "./frontdoor/", "TestPressureLoop_TheIncidentReachesTheJournal"},
		"pressure-clears-reach-the-journal":                  {"frontdoor/pressure_loop.go", "./frontdoor/", "TestPressureLoop_TheClearIsEmittedToo"},
		"pressure-the-tick-is-waited-for":                    {"frontdoor/pressure_loop.go", "./frontdoor/", "TestPressureLoop_CloseWaitsForTheTick"},
		"pressure-a-throttled-source-is-credential":          {"core/pressure/readings.go", "./core/pressure/", "TestReadings_AThrottledSourceIsCredentialAndThenGoes"},
		"pressure-unconfigured-caps-emit-nothing":            {"core/pressure/readings.go", "./core/pressure/", "TestReadings_UnconfiguredCapsProduceNoRows"},
		"pressure-stale-subjects-expire":                     {"core/pressure/dimension.go", "./core/pressure/", "TestDimension_StaleSubjectsAreDroppedOnTheTickThatReads"},
		"pressure-the-remainder-is-rendered":                 {"core/pressure/dimension.go", "./core/pressure/", "TestDimension_TheSummaryNamesWhatItLeftOut"},
		"pressure-ties-are-broken":                           {"core/pressure/dimension.go", "./core/pressure/", "TestDimension_TiesAreBrokenLexicographicallyAndRepeatably"},
		"pressure-the-recent-survive":                        {"core/pressure/dimension.go", "./core/pressure/", "TestDimension_TheLeastRecentIsDropped"},
		"pressure-dimensions-are-capped":                     {"core/pressure/dimension.go", "./core/pressure/", "TestDimension_ItStaysBoundedUnderAFloodOfSubjects"},
		"pressure-a-read-evicts":                             {"core/pressure/window.go", "./core/pressure/", "TestWindow_ItReturnsToZeroWithoutAnyFurtherWrites"},
		"pressure-the-window-slides":                         {"core/pressure/window.go", "./core/pressure/", "TestWindow_TheOldestBucketFallsOutFirst"},
		"pressure-a-straddling-burst-sums":                   {"core/pressure/window.go", "./core/pressure/", "TestWindow_ABurstAcrossABoundarySums"},
		"pressure-a-vanished-subject-still-clears":           {"core/pressure/pressure.go", "./core/pressure/", "TestPressure_ASignalWhoseSubjectIsGoneIsCleared"},
		"pressure-classes-stay-apart":                        {"core/pressure/pressure.go", "./core/pressure/", "TestPressure_TheTwoClassesRaiseAndClearIndependently"},
		"pressure-a-clear-carries-its-threshold":             {"core/pressure/pressure.go", "./core/pressure/", "TestPressure_TheOccupancyBoundaryIsExact"},
		"pressure-hysteresis-is-two-numbers":                 {"core/pressure/pressure.go", "./core/pressure/", "TestPressure_AHoveringFigureRaisesOnceAndClearsOnce"},
		"a-demand-unit-is-spent-where-it-is-checked":         {"core/exec/demand_reclaim.go", "./core/exec/", "TestDemandRetry_ConcurrentOffersSpendOneWaiterOnce"},
		"a-finalisation-is-consumed-not-just-checked":        {"core/exec/demand_reclaim.go", "./core/exec/", "TestDemandFinalisation_ManyCallersPresentingOneNoticeYieldOneOwner"},
		"a-finalisation-checks-the-ending-it-claims":         {"core/exec/demand_reclaim.go", "./core/exec/", "TestDemandFinalisation_ItIsRefusedAgainstAnEndingItDoesNotOwn"},
		"an-offer-covers-only-a-wait":                        {"frontdoor/session_loop.go", "./frontdoor/", "TestDemandOfferWindow_NoOfferCoversAFrameTheClientAlreadyWon"},
		"demand-retries-when-a-holder-becomes-askable":       {"core/exec/demand_reclaim.go", "./core/exec/", "TestDemandRetry_AHolderThatBecomesAskableServesTheRequestThatAlreadyAsked"},
		"a-departed-request-leaves-no-demand":                {"core/exec/scheduler.go", "./core/exec/", "TestDemandRetry_ACancelledRequestLeavesNoDemandBehind"},
		// ---- L6b: demand reclamation ----
		"demand-is-wired-to-the-scheduler":             {"core/exec/engine.go", "./core/exec/", "TestDemandReclaim_TheEngineWiresItToTheScheduler"},
		"only-an-untroubled-holder-is-chosen":          {"core/exec/demand_reclaim.go", "./core/exec/", "TestDemandReclaim_OnlyAnUntroubledIdleHolderIsChosen"},
		"predicate-and-reservation-are-one-hold":       {"core/exec/demand_reclaim.go", "./core/exec/", "TestDemandReclaim_NothingCanSlipBetweenJudgingAndClaiming"},
		"the-longest-silent-holder-is-chosen":          {"core/exec/demand_reclaim.go", "./core/exec/", "TestDemandReclaim_TheLongestSilentHolderIsChosen"},
		"a-stale-generation-is-refused":                {"core/exec/demand_reclaim.go", "./core/exec/", "TestDemandReclaim_AStaleGenerationCannotEndAReplacementSession"},
		"a-stale-receive-token-is-refused":             {"core/exec/demand_reclaim.go", "./core/exec/", "TestDemandReclaim_AStaleReceiveTokenIsIgnored"},
		"no-offer-without-a-knock":                     {"core/exec/demand_reclaim.go", "./core/exec/", "TestDemandReclaim_NoOfferIsIssuedWithoutAKnock"},
		"the-record-keeps-selection-time-state":        {"core/exec/demand_reclaim.go", "./core/exec/", "TestDemandReclaim_AHolderOfObjectsIsEndedAndTheRecordSaysSo"},
		"the-knock-spares-the-write-deadline":          {"frontdoor/demand_wake.go", "./frontdoor/", "TestDemandKnock_TouchesTheReadDeadlineOnly"},
		"the-offer-is-retired-at-the-wait":             {"frontdoor/session_loop.go", "./frontdoor/", "TestDrivenDemand_AnIdleClientIsToldBeforeTheConnectionEnds"},
		"finalisation-is-total":                        {"frontdoor/demand_wake.go", "./frontdoor/", "TestDrivenDemand_AnUndeclaredOutcomeStillReleasesTheLease"},
		"the-frame-precedes-the-release":               {"frontdoor/demand_wake.go", "./frontdoor/", "TestDrivenDemand_TheReleaseHappensPromptlyAfterTheFlush"},
		"reclamation-is-a-control-not-a-refusal":       {"frontdoor/demand_terminal.go", "./frontdoor/", "TestDemandReclaimed_ItsOutcomeIsNotAHeldObjectCondition"},
		"reclamation-is-charged-to-nobody":             {"frontdoor/demand_terminal.go", "./frontdoor/", "TestDemandReclaimed_ItsOutcomeIsNotAHeldObjectCondition"},
		"serve-line-at-enqueue":                        {"core/exec/scheduler.go", "./core/exec/", "TestScheduler_AnEligibleNewcomerIsServedAtEnqueueTime"},
		"transaction-bound-is-a-deadline":              {"core/exec/session.go", "./core/exec/", "TestScheduler_AnExpiredTransactionIsNotAReasonToRefuse"},
		"cancellation-undoes-its-admission":            {"core/exec/scheduler.go", "./core/exec/", "TestScheduler_ACancellationThatLosesToAGrantUndoesTheAdmission"},
		"line-skips-the-ineligible":                    {"core/exec/scheduler.go", "./core/exec/", "TestScheduler_AFullTargetDoesNotBlockTheRestOfTheLine"},
		"timeout-unwraps-alone":                        {"core/exec/scheduler.go", "./core/exec/", "TestAdmissionWait_TheBlockingCapIsDiagnosisAndNotAnIdentity"},
		"server-wait-uses-its-seam":                    {"core/exec/scheduler.go", "./core/exec/", "TestScheduler_TheServerWaitExpiresWithItsOwnIdentity"},
		"the-caller-owns-an-exact-tie":                 {"core/exec/scheduler.go", "./core/exec/", "TestScheduler_OneBoundOwnsTheWaitDeterministically"},
		"release-serves-the-line":                      {"core/exec/session.go", "./core/exec/", "TestScheduler_AReleaseNeverLeavesAnAdmittableWaiterWaiting"},
		"expired-wait-names-its-blocker":               {"core/exec/wire_session.go", "./core/exec/", "TestOpenWireSession_AWaitThatExpiresIsRecordedAsAWaitNotAsACapRefusal"},
		"the-wait-arm-comes-first":                     {"core/exec/wire_session.go", "./core/exec/", "TestAdmissionDenial_TheWaitOutranksTheCapItWaitedOn"},
		"coordinates-come-from-the-code":               {"internal/gatematrix/coords.go", "./internal/gatematrix/", "TestCoordinates_AMovedUseIsCorrected"},
		"the-matrix-is-current":                        {"docs/admission-gate-matrix.md", "./internal/gatematrix/", "TestCoordinates_TheMatrixIsWhatTheGeneratorWouldWrite"},
		"identity-excludes-git":                        {"internal/gateidentity/identity.go", "./internal/gateidentity/", "TestIdentity_AWorktreeGitFileIsNotPartOfTheFingerprint"},
		"cli-guards-the-recorded-manifest":             {"internal/gateidentity/cmd/identity/main.go", "./internal/gateidentity/cmd/identity/", "TestCLI_RecordingIntoTheRootIsRefused"},
		"cli-guards-the-against-manifest":              {"internal/gateidentity/cmd/identity/main.go", "./internal/gateidentity/cmd/identity/", "TestCLI_CheckingAgainstAManifestInTheRootIsRefused"},
		"cli-refuses-two-authorities":                  {"internal/gateidentity/cmd/identity/main.go", "./internal/gateidentity/cmd/identity/", "TestCLI_ExpectAndAgainstTogetherAreRefused"},
		"cli-validates-the-head":                       {"internal/gateidentity/cmd/identity/main.go", "./internal/gateidentity/cmd/identity/", "TestCLI_ShortHeadIsRefused"},
		"cli-validates-the-base":                       {"internal/gateidentity/cmd/identity/main.go", "./internal/gateidentity/cmd/identity/", "TestCLI_ShortBaseIsRefused"},
		"cli-refuses-a-forged-note":                    {"internal/gateidentity/cmd/identity/main.go", "./internal/gateidentity/cmd/identity/", "TestCLI_NoteWithLineBreakIsRefused"},
		"identity-requires-a-digest":                   {"internal/gateidentity/identity.go", "./internal/gateidentity/", "TestIdentity_AManifestWithNoDigestIsRefused"},
		"identity-verifies-the-body":                   {"internal/gateidentity/identity.go", "./internal/gateidentity/", "TestIdentity_AManifestWithAnEditedBodyIsRefused"},
		"identity-refuses-an-empty-manifest":           {"internal/gateidentity/identity.go", "./internal/gateidentity/", "TestIdentity_AnEmptyManifestIsRefused"},
		"identity-keeps-evidence-outside-the-root":     {"internal/gateidentity/identity.go", "./internal/gateidentity/", "TestIdentity_EvidenceInsideTheRootIsRefused"},
		"identity-refuses-two-authorities":             {"internal/gateidentity/identity.go", "./internal/gateidentity/", "TestIdentity_AManifestWithTwoDigestHeadersIsRefused"},
		"source-tree-refusal-borrows-its-precondition": {"internal/gatemutation/runner_meta_test.go", "./internal/gatemutation/", "TestRunner_ASourceTreeIsRefused"},
		"containment-probe-trusts-signal-zero":         {"internal/gatemutation/cmd/mutate/containment_test.go", "./internal/gatemutation/cmd/mutate/", "TestRunBounded_KillsTheWholeProcessTree"},

		// L1's other half: the connection card carries the budget.
		"card-a-ceiling-shows-its-figure":        {"tui/conncard.go", "./tui/", "TestCardBudget_TheCeilingsCarryTheirScope"},
		"card-an-unreported-ceiling-is-not-zero": {"tui/conncard.go", "./tui/", "TestCardBudget_AnUnreportedCeilingSaysSoRatherThanSayingZero"},
		"card-does-no-arithmetic":                {"tui/conncard.go", "./tui/", "TestCardBudget_ItDoesNoArithmetic"},
		"card-advertises-no-per-source-cap":      {"tui/conncard.go", "./tui/", "TestCardBudget_NoPerSourceConcurrencyCapIsAdvertised"},
		"card-tells-nobody-to-configure-a-pool":  {"tui/conncard.go", "./tui/", "TestCardBudget_ItTellsNobodyToConfigureAPool"},
		"card-the-connections-own-bound-leads":   {"tui/conncard.go", "./tui/", "TestCardBudget_TheConnectionsOwnBoundLeads"},
		"card-shows-no-live-availability":        {"tui/conncard.go", "./tui/", "TestCardBudget_ItShowsNoLiveAvailability"},
		"card-an-unusable-token-gets-no-advice":  {"tui/conncard.go", "./tui/", "TestCardBudget_AnUnusableTokenGetsNoBudgetBlock"},
	}
}

// declaredTests maps every Test function name to the packages declaring it.
func declaredTests(t *testing.T) map[string]map[string]bool {
	t.Helper()
	re := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	names := map[string]map[string]bool{}
	err := filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		pkg := "./" + filepath.ToSlash(filepath.Dir(strings.TrimPrefix(path, repoRoot+"/"))) + "/"
		for _, m := range re.FindAllStringSubmatch(string(body), -1) {
			if names[m[1]] == nil {
				names[m[1]] = map[string]bool{}
			}
			names[m[1]][pkg] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("no test functions found at all; the walk is broken, not the controls")
	}
	return names
}
