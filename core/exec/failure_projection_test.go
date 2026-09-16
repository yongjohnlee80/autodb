package exec

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/golib/dao"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"
	"github.com/yongjohnlee80/golib/dao/sqlite"
	"net"
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
				d := NewDialFailureAt(42, stage, shape.cause)

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

	d := NewDialFailureAt(7, DialStage(marker), errors.New("cause"))
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

// THE CONNECTION ID AN OPERATOR READS IS THE ONE THE ROW HAS, AND PRODUCTION
// PUTS IT THERE.
//
// THIS IS THE CELL THE LAST ROUND WAS MISSING, AND THE MISS IS INSTRUCTIVE.
// The projection carried a connection id, and the only caller that ever set
// one was the cell asserting ids can be set. So it proved the field COULD hold
// a row and not that any production path DOES, and every failure an operator
// actually saw said conn=0. A cell that supplies the value it is checking is
// not checking anything.
//
// NOTHING BELOW NAMES AN ID. Each case drives the real acquisition path and
// then asserts the id equals the fixture's own row, so the only way to pass is
// for production to have propagated it.
func TestAcquireRequestBackend_EveryFailurePathCarriesTheRealConnectionID(t *testing.T) {
	for _, tc := range []struct {
		name  string
		arm   func(t *testing.T, f *fixture)
		stage DialStage
	}{
		{
			"the target pool will not build",
			func(t *testing.T, f *fixture) {
				orig := openSQLite
				t.Cleanup(func() { openSQLite = orig })
				openSQLite = func(_ context.Context, _, _ string, _ ...sqlite.Option) (dao.DataConn, error) {
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
				}
			},
			"", // a configuration failure, checked separately below
		},
		{
			// THE ONLY PRODUCTION ROUTE TO requestTargetFailure's DialFailure
			// ARM. Everything openTarget itself rejects is typed as a
			// configuration failure now, so the arm that wraps a raw target
			// error is reached by what happens BEFORE that: decrypting the
			// stored DSN. A corrupted secret is the honest way to drive it --
			// and without this case, removing propagation from that arm
			// changed nothing observable, which is how a dead-looking path
			// stays untested until it is not dead.
			"the stored secret will not decrypt",
			func(t *testing.T, f *fixture) {
				if err := f.store.Connections.OnCtx(context.Background()).
					With(meta.ConnID, f.connID).
					Set(meta.ConnDSNEnc, []byte("not a sealed secret")).Update(); err != nil {
					t.Fatalf("corrupting the stored DSN: %v", err)
				}
			},
			DialStageUnclassified,
		},
		{
			"the physical pin fails",
			func(t *testing.T, f *fixture) {
				orig := pinSessionConn
				t.Cleanup(func() { pinSessionConn = orig })
				pinSessionConn = func(context.Context, dao.DataConn) (golibpg.PinnedConn, error) {
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
				}
			},
			DialStageConnect,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t)
			row, err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).Get()
			if err != nil {
				t.Fatalf("reading the connection row: %v", err)
			}
			f.eng.closeTarget(f.connID)
			tc.arm(t, f)
			// Re-read: an arm may have changed the row, and acquisition must
			// be driven with what production would actually load.
			if fresh, ferr := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).Get(); ferr == nil {
				row = fresh
			}

			_, aerr := f.eng.acquireRequestBackend(ctx, &session{connID: f.connID}, row)
			if aerr == nil {
				t.Fatal("the armed failure did not fail the acquisition")
			}

			if d, ok := DialFailureOf(aerr); ok {
				if d.ConnID() != f.connID {
					t.Errorf("DialFailure carries conn=%d, want the row's own id %d; an "+
						"operator reading this event cannot find the connection",
						d.ConnID(), f.connID)
				}
				if !strings.Contains(d.AuditDetail(), fmt.Sprintf("conn=%d", f.connID)) {
					t.Errorf("AuditDetail = %q, want conn=%d", d.AuditDetail(), f.connID)
				}
				if strings.Contains(d.AuditDetail(), "conn=0") {
					t.Errorf("AuditDetail = %q says conn=0 for a connection whose row was "+
						"known all along", d.AuditDetail())
				}
				if tc.stage != "" && d.Stage() != tc.stage {
					t.Errorf("stage = %q, want %q", d.Stage(), tc.stage)
				}
				return
			}
			if c, ok := ConfigFailureOf(aerr); ok {
				if c.ConnID() != f.connID {
					t.Errorf("ConfigFailure carries conn=%d, want the row's own id %d",
						c.ConnID(), f.connID)
				}
				if strings.Contains(c.AuditDetail(), "conn=0") {
					t.Errorf("AuditDetail = %q says conn=0 for a known row", c.AuditDetail())
				}
				return
			}
			t.Fatalf("err = %v, want a typed failure carrying the connection id", aerr)
		})
	}
}

// The settings stage has its own raise site and its own chance to forget.
func TestPinTargetBackend_ASanitationFailureCarriesTheRealConnectionID(t *testing.T) {
	c := &destroyingConn{gateConn: *newGateConn()}
	c.targetErr["UNLISTEN *"] = &pgconn.PgError{Message: "the target refuses this reset"}

	orig := pinSessionConn
	t.Cleanup(func() { pinSessionConn = orig })
	pinSessionConn = func(context.Context, dao.DataConn) (golibpg.PinnedConn, error) { return c, nil }

	e := &Engine{}
	s := gateSession()
	s.connID = 77

	_, err := e.pinTargetBackend(context.Background(), s, nil)
	d, ok := DialFailureOf(err)
	if !ok {
		t.Fatalf("err = %v, want a DialFailure at the settings stage", err)
	}
	if d.Stage() != DialStageSettings {
		t.Errorf("stage = %q, want %q", d.Stage(), DialStageSettings)
	}
	if d.ConnID() != 77 {
		t.Errorf("conn = %d, want 77 — the settings raise site knows the session's "+
			"connection and must say which one could not be sanitised", d.ConnID())
	}
}
