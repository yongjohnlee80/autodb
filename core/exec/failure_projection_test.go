package exec

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// dialCauseShapes are the ways a real driver reports a failed connection, each
// with every token that must never leave this process.
//
// TAKEN FROM WHAT DRIVERS ACTUALLY PRODUCE. pgx's connect error is a
// key=value line naming the host, the role and the database, and for a URL DSN
// it reproduces the connection string entire. Measured against the previous
// projection, every token below appeared in BOTH Error and AuditDetail, and
// AuditDetail is what session_loop publishes as EventDialFailed.Detail.
var dialCauseShapes = []struct {
	name    string
	cause   error
	secrets []string
}{
	{
		"a pgx keyword connect error",
		errors.New("failed to connect to `host=db7.internal user=autodb_rw database=billing`: " +
			"server error (FATAL: password authentication failed for user \"autodb_rw\" (SQLSTATE 28P01))"),
		[]string{"db7.internal", "autodb_rw", "billing", "28P01"},
	},
	{
		"a URL DSN with a plaintext password",
		errors.New(`cannot parse "postgres://autodb_rw:hunter2@db7.internal:6432/billing": invalid port`),
		[]string{"autodb_rw", "hunter2", "db7.internal", "6432", "billing"},
	},
	{
		"a URL DSN with a percent-encoded password",
		errors.New(`cannot parse "postgres://u:p%40ssw0rd%21@db7.internal:1/d"`),
		[]string{"p%40ssw0rd%21", "db7.internal"},
	},
	{
		"a PAT in the username position",
		errors.New(`cannot parse "postgres://adb_pat_aaaaaaaaaa.bbbbbbbb@db7.internal:1/d"`),
		[]string{"adb_pat_aaaaaaaaaa.bbbbbbbb", "db7.internal"},
	},
	{
		"a secret passed as a query parameter",
		errors.New(`cannot parse "postgres://u@h:1/d?sslmode=bogus&password=second_secret"`),
		[]string{"second_secret"},
	},
}

// A DIAL FAILURE PROJECTS FIXED VALUES AT EVERY STAGE, AND THE CAUSE AT NONE.
//
// Driven across every stage rather than one, because the stage is the only
// thing that varies in the projection and a cell fixing it to one value would
// not notice a stage-specific arm being added that formats the cause.
func TestDialFailure_NoCauseReachesAnyProjection(t *testing.T) {
	for _, shape := range dialCauseShapes {
		for _, stage := range dialStages() {
			t.Run(fmt.Sprintf("%s/%s", shape.name, stage), func(t *testing.T) {
				d := NewDialFailureAt(stage, shape.cause).ForConnection(42)

				for _, out := range []struct{ name, value string }{
					{"Error", d.Error()},
					{"AuditDetail", d.AuditDetail()},
				} {
					for _, secret := range shape.secrets {
						if strings.Contains(out.value, secret) {
							t.Errorf("%s carries %q\n  full: %s", out.name, secret, out.value)
						}
					}
					if strings.Contains(out.value, "postgres://") {
						t.Errorf("%s carries a connection string\n  full: %s", out.name, out.value)
					}
				}

				// WHAT MUST REMAIN. A projection that leaks nothing and says
				// nothing sends an operator to read code.
				det := d.AuditDetail()
				for _, want := range []string{"stage=" + string(stage), "attempts=1", "conn=42"} {
					if !strings.Contains(det, want) {
						t.Errorf("AuditDetail = %q, want it to carry %q", det, want)
					}
				}
				if d.Cause() == nil {
					t.Error("the cause was discarded; it is the operator's only way in from a debugger")
				}
			})
		}
	}
}

// A TYPED-STRING VALUE INVENTED OUTSIDE THE CLOSED SET NEVER REACHES A
// PROJECTION.
//
// THIS IS THE HOLE THE AST WALK COULD NOT SEE. That walk reads constructor
// arguments in ONE package. DialStage and ConfigStage are defined string
// types, so DialStage(secret) compiles anywhere, and while the fields were
// exported a caller could also assign one after the fact. Fields are private
// now and the constructors normalize, so both routes end at a fixed literal.
func TestFailures_AnInventedTypedStringNormalizesToUnclassified(t *testing.T) {
	const marker = "host=db7.internal password=hunter2"

	d := NewDialFailureAt(DialStage(marker), errors.New("cause")).ForConnection(7)
	if d.Stage() != DialStageUnclassified {
		t.Errorf("stage = %q, want %q", d.Stage(), DialStageUnclassified)
	}
	for _, v := range []string{d.Error(), d.AuditDetail()} {
		if strings.Contains(v, marker) {
			t.Errorf("an invented stage reached a projection: %q", v)
		}
	}

	c := NewConfigFailure(ConfigStage(marker), 7, ConfigDetail(marker), errors.New("cause"))
	if c.Stage() != ConfigStageUnclassified {
		t.Errorf("stage = %q, want %q", c.Stage(), ConfigStageUnclassified)
	}
	if c.Detail() != DetailUnclassified {
		t.Errorf("detail = %q, want %q", c.Detail(), DetailUnclassified)
	}
	for _, v := range []string{c.Error(), c.AuditDetail(), c.SafeLog()} {
		if strings.Contains(v, marker) {
			t.Errorf("an invented detail reached a projection: %q", v)
		}
	}
}
