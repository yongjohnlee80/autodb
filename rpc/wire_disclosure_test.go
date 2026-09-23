package rpc

import (
	"errors"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodb/core/exec"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
)

// leakyCause is shaped like a real driver connect error: it names the target
// (what an operator needs) AND carries a credential (what they must not be
// shown). Both halves are asserted separately in every case below.
const leakyCause = "failed to connect to `postgres://autodb_rw:s3cr3tP@db7.internal:6432/billing?sslpassword=hunter2`: dial error: connection refused"

var (
	causeSecrets    = []string{"s3cr3tP", "hunter2"}
	causeDiagnostic = []string{"db7.internal", "6432", "billing", "connection refused"}
)

func wireError(t *testing.T, err error) *golibrpc.Error {
	t.Helper()
	var e *golibrpc.Error
	if !errors.As(err, &e) {
		t.Fatalf("wireErr = %T (%v), want *golibrpc.Error", err, err)
	}
	return e
}

// On a host-local surface the cause is disclosed — this is the POSITIVE
// CONTROL for the withholding test below. Without it, that test would pass
// just as well against plumbing that never carries a cause at all.
func TestWireErr_DialFailure_HostLocalDisclosesScrubbedCause(t *testing.T) {
	t.Parallel()
	de := exec.NewDialFailure(7, errors.New(leakyCause))
	e := wireError(t, (&Server{discloseDetail: true}).wireErr(de))

	if e.Code != CodeDialFailed {
		t.Errorf("code = %d, want CodeDialFailed (%d)", e.Code, CodeDialFailed)
	}
	for _, want := range causeDiagnostic {
		if !strings.Contains(e.Message, want) {
			t.Errorf("host-local disclosure lost the diagnosis %q:\n  %s", want, e.Message)
		}
	}
	for _, bad := range causeSecrets {
		if strings.Contains(e.Message, bad) {
			t.Errorf("host-local disclosure leaked the secret %q:\n  %s", bad, e.Message)
		}
	}
}

// Off-host, only the cause-free sentinel shape crosses. Asserted as EQUALITY
// with DialFailure.Error(), not merely "the secret is absent": a message that
// dropped the diagnosis too, or grew a new field, is also wrong.
func TestWireErr_DialFailure_OffHostWithholdsCause(t *testing.T) {
	t.Parallel()
	de := exec.NewDialFailure(7, errors.New(leakyCause))
	e := wireError(t, (&Server{discloseDetail: false}).wireErr(de))

	if e.Code != CodeDialFailed {
		t.Errorf("code = %d, want CodeDialFailed (%d)", e.Code, CodeDialFailed)
	}
	if e.Message != de.Error() {
		t.Errorf("off-host message is not the cause-free shape:\n got  %s\n want %s", e.Message, de.Error())
	}
	for _, bad := range append(append([]string{}, causeSecrets...), causeDiagnostic...) {
		if strings.Contains(e.Message, bad) {
			t.Errorf("off-host message leaked %q:\n  %s", bad, e.Message)
		}
	}
}

func TestWireErr_ConfigFailure_HostLocalDisclosesScrubbedCause(t *testing.T) {
	t.Parallel()
	cf := exec.NewConfigFailure(exec.ConfigStageDSN, 7, exec.DetailDSNUnusable, errors.New(leakyCause))
	e := wireError(t, (&Server{discloseDetail: true}).wireErr(cf))

	if e.Code != CodeConfigFailed {
		t.Errorf("code = %d, want CodeConfigFailed (%d)", e.Code, CodeConfigFailed)
	}
	for _, want := range causeDiagnostic {
		if !strings.Contains(e.Message, want) {
			t.Errorf("host-local disclosure lost the diagnosis %q:\n  %s", want, e.Message)
		}
	}
	for _, bad := range causeSecrets {
		if strings.Contains(e.Message, bad) {
			t.Errorf("host-local disclosure leaked the secret %q:\n  %s", bad, e.Message)
		}
	}
}

func TestWireErr_ConfigFailure_OffHostWithholdsCause(t *testing.T) {
	t.Parallel()
	cf := exec.NewConfigFailure(exec.ConfigStageDSN, 7, exec.DetailDSNUnusable, errors.New(leakyCause))
	e := wireError(t, (&Server{discloseDetail: false}).wireErr(cf))

	if e.Code != CodeConfigFailed {
		t.Errorf("code = %d, want CodeConfigFailed (%d)", e.Code, CodeConfigFailed)
	}
	if e.Message != cf.Error() {
		t.Errorf("off-host message is not the cause-free shape:\n got  %s\n want %s", e.Message, cf.Error())
	}
	for _, bad := range append(append([]string{}, causeSecrets...), causeDiagnostic...) {
		if strings.Contains(e.Message, bad) {
			t.Errorf("off-host message leaked %q:\n  %s", bad, e.Message)
		}
	}
}

// The two typed failures are the ONLY surface-dependent arms. Everything else
// must still reach the package-level mapping unchanged, on either surface —
// otherwise the method silently became a second disclosure policy.
func TestWireErr_MethodDelegatesSurfaceIndependentMapping(t *testing.T) {
	t.Parallel()
	for _, disclose := range []bool{false, true} {
		s := &Server{discloseDetail: disclose}

		// A mapped sentinel keeps its established code and constant text.
		e := wireError(t, s.wireErr(exec.ErrEmptyStatement))
		if e.Code != CodeStatementRejected || e.Message != exec.ErrEmptyStatement.Error() {
			t.Errorf("discloseDetail=%v: sentinel mapping changed: %+v", disclose, e)
		}

		// An unmapped error still passes through untouched, so the transport
		// withholds it. Disclosure must not widen this.
		unmapped := errors.New("dsn parse: postgres://user:SECRET@10.0.0.5/prod")
		if got := s.wireErr(unmapped); got != unmapped {
			t.Errorf("discloseDetail=%v: unmapped error transformed: %v", disclose, got)
		}
	}
}
