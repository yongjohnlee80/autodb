package exec

// The checkout verifier, after it stopped issuing a statement.
//
// It now reads standard_conforming_strings from the ParameterStatus the server
// reports rather than running `SHOW`. That removes autodb's objects from the
// namespace it hands to wire clients -- but a check that no longer runs a query
// is also a check that could silently stop checking, and "no query" and "no
// verification" look identical from outside. So these cells assert the property
// still holds, against a target that really does report the wrong value.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func parsingGUCDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_PGURL")
	if dsn == "" {
		t.Skip("TEST_PGURL not set; skipping the live parsing-mode tests")
	}
	return dsn
}

// A TARGET THAT REPORTS standard_conforming_strings=off IS REFUSED.
//
// The database default is changed, so every NEW session reports `off` at
// startup -- which is the shape a hostile or misconfigured target has, and the
// one the verifier exists for. ALTER DATABASE does not disturb sessions that
// already exist, so the change is invisible to anything but connections opened
// inside this cell.
//
// WHAT THIS CELL DEFENDS. The fix deleted a probe and replaced a query with a
// read of reported state. Deleting the check entirely, or reading a parameter
// name that is never reported (so the value is always empty), would both leave
// autodb serving statements it cannot parse the same way the server does. This
// cell fails for all three.
func TestParsingGUC_ATargetReportingOffIsRefusedAtCheckout(t *testing.T) {
	dsn := parsingGUCDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())

	var dbName string
	if err := admin.QueryRow(ctx, "SELECT current_database()").Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	// Quote the identifier: the harness's database name contains a hyphen.
	set := func(val string) {
		if _, err := admin.Exec(ctx,
			`ALTER DATABASE "`+dbName+`" SET standard_conforming_strings = `+val); err != nil {
			t.Fatalf("ALTER DATABASE ... %s: %v", val, err)
		}
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Logf("cleanup: reconnect: %v", err)
			return
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(),
			`ALTER DATABASE "`+dbName+`" RESET standard_conforming_strings`); err != nil {
			t.Logf("cleanup: RESET: %v", err)
		}
	})
	set("off")

	// Prove the instrument: a NEW session really does report off now. Without
	// this the cell passes for a target that never drifted, which is no
	// evidence about the verifier at all.
	probe, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	reported := probe.PgConn().ParameterStatus("standard_conforming_strings")
	probe.Close(context.Background())
	if !strings.EqualFold(reported, "off") {
		t.Fatalf("the target reports standard_conforming_strings=%q; this cell cannot "+
			"observe the verifier unless the target really drifted", reported)
	}

	conn, err := openPostgres(ctx, "parsing-guc-off", dsn, pgPrepareConnVerify())
	if err != nil {
		return // refused at open: fail-closed, which is the point
	}
	defer conn.Close()
	got, qerr := scalarStringQ(ctx, conn, "SELECT 1")
	if qerr == nil {
		t.Fatalf("a target reporting standard_conforming_strings=off served a statement; "+
			"the checkout verification is not enforcing anything (got %q)", got)
	}
	t.Logf("refused, as it must be: %v", qerr)
}

// AND A TARGET REPORTING `on` IS SERVED.
//
// The positive control for the cell above: without it, a verifier that refused
// EVERY connection would satisfy the refusal cell perfectly.
func TestParsingGUC_ATargetReportingOnIsServed(t *testing.T) {
	dsn := parsingGUCDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, err := openPostgres(ctx, "parsing-guc-on", dsn, pgPrepareConnVerify())
	if err != nil {
		t.Fatalf("a compatible target was refused at open: %v", err)
	}
	defer conn.Close()
	if _, err := scalarStringQ(ctx, conn, "SELECT 1"); err != nil {
		t.Fatalf("a compatible target was refused at checkout: %v", err)
	}
}
