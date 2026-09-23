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

// A cause the scrubber cannot parse to the end of its password value is
// withheld ENTIRELY, even on a host-local surface. Half-masked is worse than
// absent: it reads as though it had been scrubbed.
func TestWireErr_DialFailure_UnparseableCauseFallsBackToTheShape(t *testing.T) {
	t.Parallel()
	const unterminated = "failed to connect to `user=u password='never closed and the rest is secret"
	de := exec.NewDialFailure(7, errors.New(unterminated))
	e := wireError(t, (&Server{discloseDetail: true}).wireErr(de))

	if e.Message != de.Error() {
		t.Errorf("an unparseable cause was disclosed instead of withheld:\n got  %s\n want %s",
			e.Message, de.Error())
	}
	for _, bad := range []string{"never closed", "secret", "password"} {
		if strings.Contains(e.Message, bad) {
			t.Errorf("withheld message still carries %q:\n  %s", bad, e.Message)
		}
	}
}

// The grammar fixes must be reachable THROUGH causeOrShape, not merely correct
// in the helper. Each row is a carrier form that previously leaked; each is
// asserted on the message the RPC error actually carries.
func TestWireErr_GrammarFormsAreScrubbedOnTheRealPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		cause string
		gone  []string
		kept  []string
	}{
		{
			name:  "ampersand is an ordinary keyword value byte",
			cause: "failed to connect to `user=u password=ab&cd host=db7.internal`: refused",
			gone:  []string{"ab&cd", "&cd"},
			kept:  []string{"host=db7.internal", "refused"},
		},
		{
			name:  "vertical tab around the equals",
			cause: "failed to connect to `user=u password\v=\vsecret host=db7.internal`: refused",
			gone:  []string{"secret"},
			kept:  []string{"host=db7.internal", "refused"},
		},
		{
			name:  "form feed around the equals",
			cause: "failed to connect to `user=u password\f=\fsecret host=db7.internal`: refused",
			gone:  []string{"secret"},
			kept:  []string{"host=db7.internal", "refused"},
		},
		{
			name:  "backslash carries an unquoted value past a space",
			cause: `failed to connect to ` + "`" + `user=u password=ab\ cd host=db7.internal` + "`" + `: refused`,
			gone:  []string{`ab\ cd`, " cd"},
			kept:  []string{"host=db7.internal", "refused"},
		},
		{
			name:  "url query keeps the parameters after the secret",
			cause: "dsn `postgres://u@h:5432/d?sslpassword=ab&application_name=x` unusable",
			gone:  []string{"sslpassword=ab"},
			kept:  []string{"application_name=x", "h:5432"},
		},
		{
			name:  "raw at-sign inside the userinfo password",
			cause: "dsn `postgres://user:ab@cd@host/db` unusable",
			gone:  []string{"ab@cd", "cd@host"},
			kept:  []string{"@host/db", "unusable"},
		},
		{
			name:  "raw at-sign inside the username",
			cause: "dsn `postgres://us@er:pw@host/db` unusable",
			gone:  []string{":pw@"},
			kept:  []string{"us@er", "@host/db"},
		},
		{
			name:  "percent-encoded query key still names the carrier",
			cause: "dsn `postgres://u@h/db?pass%77ord=secret&application_name=x` unusable",
			gone:  []string{"secret"},
			kept:  []string{"application_name=x", "h/db"},
		},
		{
			name:  "apostrophe is a url query value byte, not a boundary",
			cause: "dsn `postgres://u@h/db?password=ab'cd&application_name=x` unusable",
			gone:  []string{"ab'cd", "'cd"},
			kept:  []string{"application_name=x", "h/db"},
		},
		{
			name:  "backtick is a url query value byte, not a boundary",
			cause: "dsn `postgres://u@h/db?password=ab`cd&application_name=x` unusable",
			gone:  []string{"ab`cd", "cd&"},
			kept:  []string{"application_name=x", "h/db"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			de := exec.NewDialFailure(7, errors.New(tc.cause))
			e := wireError(t, (&Server{discloseDetail: true}).wireErr(de))
			if e.Message == de.Error() {
				t.Fatalf("fell back to the shape on a parseable cause; the cell would "+
					"pass for the wrong reason:\n  %s", e.Message)
			}
			for _, bad := range tc.gone {
				if strings.Contains(e.Message, bad) {
					t.Errorf("leaked %q on the real path:\n  %s", bad, e.Message)
				}
			}
			for _, want := range tc.kept {
				if !strings.Contains(e.Message, want) {
					t.Errorf("lost the diagnosis %q:\n  %s", want, e.Message)
				}
			}
		})
	}
}

// causeOrShape's own contract, both directions, so neither arm can rot into
// the other: a withholding surface never discloses even a clean cause, and a
// disclosing surface never answers the shape for one.
func TestCauseOrShape_BothDirections(t *testing.T) {
	t.Parallel()
	clean := errors.New("dial error: connection refused")
	de := exec.NewDialFailure(7, clean)

	if got := (&Server{discloseDetail: false}).causeOrShape(de, clean); got != de.Error() {
		t.Errorf("off-host answered something other than the shape: %s", got)
	}
	if got := (&Server{discloseDetail: true}).causeOrShape(de, clean); got != clean.Error() {
		t.Errorf("host-local did not answer the clean cause: %s", got)
	}
	// A nil cause has nothing to disclose and must not panic into one.
	if got := (&Server{discloseDetail: true}).causeOrShape(de, nil); got != de.Error() {
		t.Errorf("a nil cause did not answer the shape: %s", got)
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
