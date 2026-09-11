package frontdoor

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/yongjohnlee80/autodb/core/admission"
	"github.com/yongjohnlee80/autodb/core/exec"
)

// THE CLASSIFICATION ITSELF, WITH NO LIVE TARGET.
//
// The end-to-end witness for this repair needs a real PostgreSQL and SKIPS
// without one — and a skip prints `ok`, which is indistinguishable from a pass.
// A reviewer running the suite in an environment with no TEST_PGURL would be
// handed a green that proved nothing, which is the same empty-selection trap as
// a `-run` pattern matching no tests.
//
// So the classification is pinned HERE too, where it needs nothing but the
// error value. Both cells are real evidence; only one of them is conditional.
//
// IT ASSERTS THE NARROWNESS, NOT ONLY THE FIX. The retained fatal default is
// CORRECT for a transport failure or a poisoned raw face, and the repair is
// deliberately one sentinel wide. A cell that only checked the new case would go
// green if someone later widened it to every golib error — the exact
// over-claim this PR was sent back to remove.
func TestRefusalClassifier_OneSentinelIsARefusalAndTheRestStayFatal(t *testing.T) {
	for _, c := range []struct {
		name  string
		err   error
		code  string
		rule  string
		fatal bool
	}{
		{
			name:  "our own sequence refusal",
			err:   exec.ErrWireSequenceRefused,
			code:  sqlStateFeatureNotSupported,
			rule:  "frontdoor/sequence-unsupported",
			fatal: false,
		},
		{
			// THE RETAINED DEFAULT, asserted so the narrowing is visible. A lost
			// wire really is fatal: there is nothing left to be ready for.
			name:  "a genuinely lost wire stays fatal",
			err:   fmt.Errorf("%w: read: connection reset by peer", exec.ErrWireFaceLost),
			code:  sqlStateProtocolViolation,
			rule:  "frontdoor/wire-face-lost",
			fatal: true,
		},
		{
			// AN UNRECOGNISED ERROR IS NOT SILENTLY TREATED AS A REFUSAL. This is
			// the mutation-shaped case: if the classifier ever matched a category
			// rather than the one sentinel, this row would move.
			name:  "an unrecognised engine error keeps the generic refusal",
			err:   errors.New("exec: something nobody has classified"),
			code:  "42501",
			rule:  "gate/refused",
			fatal: false,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, rule, _, fatal := classifyGateError(c.err)
			if code != c.code {
				t.Errorf("code = %q, want %q", code, c.code)
			}
			if rule != c.rule {
				t.Errorf("rule = %q, want %q", rule, c.rule)
			}
			if fatal != c.fatal {
				t.Errorf("fatal = %v, want %v", fatal, c.fatal)
			}
		})
	}
}

func TestAdmissionErrorFrame_TwoDisclosureModes(t *testing.T) {
	t.Parallel()
	reason := admission.Reason{
		Code: admission.CodeStatementUnsupported, Class: admission.ClassUnsupported,
		Span: 7, Subject: "UPDATE", Detail: "profile refuses UPDATE", Hint: "change the statement", Continue: true,
	}
	post := admissionErrorFrame(reason, true)
	if post.Code != sqlStateFeatureNotSupported || post.Severity != "ERROR" ||
		post.Message != reason.Detail || post.Detail != string(reason.Code) ||
		post.Hint != reason.Hint || post.Position != 7 || post.Where != reason.Subject {
		t.Fatalf("post-disclosure frame lost Reason fields: %+v", post)
	}
	pre := admissionErrorFrame(reason, false)
	if pre.Code != DenialSQLState || pre.Message != DenialMessage || pre.Detail != "frontdoor/denied" {
		t.Fatalf("pre-disclosure frame is not uniform: %+v", pre)
	}
	if pre.Position != 0 || pre.Where != "" || pre.Hint != "" {
		t.Fatalf("pre-disclosure frame leaked Reason fields: %+v", pre)
	}
}

func TestAdmissionErrorFrame_NovelDefaultStageCodeFlowsWithoutMappings(t *testing.T) {
	t.Parallel()
	want := admission.Reason{
		Code: "future-default-rule", Class: admission.ClassProgramLimit,
		Span: 19, Subject: "future subject", Detail: "future rule refused the statement",
		Hint: "change the future shape", Continue: true,
	}
	reason, ok := exec.AdmissionReason(exec.AdmissionError(want))
	if !ok {
		t.Fatal("core discarded a novel structured refusal")
	}
	frame := admissionErrorFrame(reason, true)
	if frame.Code != sqlStateProgramLimit || frame.Severity != "ERROR" ||
		frame.Message != want.Detail || frame.Detail != string(want.Code) ||
		frame.Hint != want.Hint || frame.Position != int32(want.Span) || frame.Where != want.Subject {
		t.Fatalf("novel default-handled reason did not render generically: %+v", frame)
	}
}

func TestAdmissionOperationalError_IsOpaqueToTheClient(t *testing.T) {
	t.Parallel()
	secret := errors.New("target catalog prod-secret at 10.0.0.5")
	err := &admission.OperationalError{Stage: "readeranalysis", Cause: secret}
	code, rule, hint, fatal := classifyGateError(err)
	if code != "58000" || rule != "frontdoor/admission-unavailable" || hint == "" || fatal {
		t.Fatalf("operational classification = %q/%q/%q/fatal=%v", code, rule, hint, fatal)
	}
	message := gateMessage(err)
	if message != "the admission pipeline could not evaluate this statement" || strings.Contains(message, secret.Error()) {
		t.Fatalf("operational client message leaked its cause: %q", message)
	}
	if !errors.Is(err, secret) || !strings.Contains(err.Error(), secret.Error()) {
		t.Fatalf("server-side operational cause was lost: %v", err)
	}
}

func TestAdmissionOperationalError_LostWireRemainsFatal(t *testing.T) {
	cause := errors.New("read-only BEGIN transport failed")
	err := fmt.Errorf("%w: %w", exec.ErrWireFaceLost,
		&admission.OperationalError{Stage: "readonlyenforcement", Cause: cause})
	code, rule, _, fatal := classifyGateError(err)
	if code != sqlStateProtocolViolation || rule != "frontdoor/wire-face-lost" || !fatal {
		t.Fatalf("lost-wire operational classification = %q/%q/fatal=%v", code, rule, fatal)
	}
	if got := gateMessage(err); strings.Contains(got, cause.Error()) {
		t.Fatalf("operational cause crossed the disclosure boundary: %q", got)
	}
}

// A refusal must not be reachable by WRAPPING: errors.Is walks the chain, so a
// lost-wire error that happened to wrap the refusal sentinel would be declassed
// from fatal. Ordering in the switch is what prevents it, and ordering is
// exactly the kind of thing a later edit reshuffles without noticing.
func TestRefusalClassifier_ALostWireWrappingARefusalStaysFatal(t *testing.T) {
	err := fmt.Errorf("%w: %w", exec.ErrWireFaceLost, exec.ErrWireSequenceRefused)
	code, rule, _, fatal := classifyGateError(err)
	if !fatal {
		t.Errorf("a lost wire that wraps the refusal sentinel was classified "+
			"non-fatal (%s / %s) — the wire is gone whatever it wrapped", code, rule)
	}
}

func TestFraming_ALostWireOutranksAWrappedAdmissionReason(t *testing.T) {
	reason := admission.Reason{
		Code: admission.CodeStatementUnsupported, Class: admission.ClassUnsupported,
		Detail: "this recoverable reason must not outrank a dead wire", Continue: true,
	}
	err := fmt.Errorf("%w: %w", exec.ErrWireFaceLost, exec.AdmissionError(reason))
	for _, extended := range []bool{false, true} {
		name := "simple"
		if extended {
			name = "extended"
		}
		t.Run(name, func(t *testing.T) {
			frame, keep, closeReason, _ := frameErrorForTest(t, extended, err)
			if keep || closeReason != "frontdoor/wire-face-lost" {
				t.Fatalf("framing returned keep=%v closeReason=%q, want fatal wire close", keep, closeReason)
			}
			if frame.Severity != "FATAL" || frame.Detail != "frontdoor/wire-face-lost" ||
				strings.Contains(frame.Message, reason.Detail) {
				t.Fatalf("lost-wire frame was declassed by wrapped admission reason: %+v", frame)
			}
		})
	}
}

func TestExtendedFraming_TargetBeginErrorIsVerbatim(t *testing.T) {
	target := &pgconn.PgError{
		Severity: "ERROR", Code: "25001", Message: "transaction rejected",
		Detail: "target detail", Hint: "target hint", Position: 17,
		File: "xact.c", Line: 42, Routine: "BeginTransactionBlock",
	}
	frame, keep, closeReason, seg := frameErrorForTest(t, true, target)
	if !keep || closeReason != "" || !seg.discarding {
		t.Fatalf("target rejection returned keep=%v closeReason=%q discarding=%v, want recoverable discard", keep, closeReason, seg.discarding)
	}
	if frame.Code != target.Code || frame.Message != target.Message || frame.Detail != target.Detail ||
		frame.Hint != target.Hint || frame.Position != target.Position || frame.File != target.File ||
		frame.Line != target.Line || frame.Routine != target.Routine {
		t.Fatalf("target BEGIN error was not forwarded verbatim: got %+v, want %+v", frame, target)
	}
}

func TestExtendedFraming_OperationalTargetErrorStaysOpaque(t *testing.T) {
	target := &pgconn.PgError{Code: "XX000", Message: "secret target catalog failure"}
	err := &admission.OperationalError{Stage: "readonlyenforcement", Cause: target}
	frame, keep, closeReason, seg := frameErrorForTest(t, true, err)
	if !keep || closeReason != "" || !seg.discarding {
		t.Fatalf("operational failure returned keep=%v closeReason=%q discarding=%v", keep, closeReason, seg.discarding)
	}
	if frame.Code != "58000" || strings.Contains(frame.Message, target.Message) || strings.Contains(frame.Detail, target.Code) {
		t.Fatalf("operational target cause crossed the disclosure boundary: %+v", frame)
	}
}

func frameErrorForTest(t *testing.T, extended bool, err error) (*pgproto3.ErrorResponse, bool, string, *segmentLane) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	frames := make(chan *pgproto3.ErrorResponse, 1)
	readErr := make(chan error, 1)
	go func() {
		msg, rerr := pgproto3.NewFrontend(client, client).Receive()
		if rerr != nil {
			readErr <- rerr
			return
		}
		frame, ok := msg.(*pgproto3.ErrorResponse)
		if !ok {
			readErr <- fmt.Errorf("received %T, want ErrorResponse", msg)
			return
		}
		frames <- frame
	}()

	l, _, _ := listenerWith(t, Options{
		Authn: &fakeAuth{result: goodSession()}, Queries: okQueries(), AuthFailuresPerIP: unthrottled,
	})
	be := pgproto3.NewBackend(newFrameReader(server), server)
	closeReason := ""
	seg := &segmentLane{}
	keep := false
	if extended {
		keep = l.frameExtendedError(server, be, exec.WireSessionResult{}, err, "peer", seg, &closeReason)
	} else {
		keep = l.frameGateError(server, be, exec.WireSessionResult{}, err, "peer", &closeReason)
	}

	select {
	case frame := <-frames:
		return frame, keep, closeReason, seg
	case err := <-readErr:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading framed error")
	}
	return nil, false, "", nil
}
