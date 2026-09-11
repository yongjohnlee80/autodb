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

	// WHICHEVER STAGE REFUSES, the refusal has to be THE ONE THIS CELL NAMES.
	//
	// An earlier version returned as soon as openPostgres returned any error at
	// all, on the reasoning that a refusal is a refusal. Review caught it and a
	// decoy confirmed it: pointed at a dead port, the cell passed on
	// "connect: connection refused" -- so a broken DSN, a TLS failure or a pool
	// regression all counted as proof that the grammar check works. It asserted
	// that something went wrong, which is not the claim.
	refusal := func() error {
		conn, oerr := openPostgres(ctx, "parsing-guc-off", dsn, pgPrepareConnVerify())
		if oerr != nil {
			return oerr
		}
		defer conn.Close()
		got, qerr := scalarStringQ(ctx, conn, "SELECT 1")
		if qerr == nil {
			t.Fatalf("a target reporting standard_conforming_strings=off served a "+
				"statement; the checkout verification is not enforcing anything (got %q)", got)
		}
		return qerr
	}()
	if !isCheckoutRefusal(refusal) {
		t.Fatalf("the target was refused, but not by the checkout verification:\n  %v\n"+
			"Only a refusal from the hook is evidence here; anything else means the "+
			"connection failed for a reason this cell does not test.", refusal)
	}
	t.Logf("refused by the checkout hook, as it must be: %v", refusal)

	// SPECIFICITY. isCheckoutRefusal has to tell the hook's refusal apart from
	// an ordinary connection failure, or the assertion above is the bare
	// err != nil it replaced. A DSN nothing answers must NOT satisfy it.
	dead := func() error {
		conn, oerr := openPostgres(ctx, "parsing-guc-dead",
			"postgres://nobody:nope@127.0.0.1:1/nosuchdb?sslmode=disable", pgPrepareConnVerify())
		if oerr != nil {
			return oerr
		}
		defer conn.Close()
		_, qerr := scalarStringQ(ctx, conn, "SELECT 1")
		return qerr
	}()
	if dead == nil {
		t.Fatal("a DSN pointing at a dead port succeeded; the decoy proves nothing")
	}
	if isCheckoutRefusal(dead) {
		t.Errorf("an unreachable target reads as a checkout refusal:\n  %v\n"+
			"the predicate does not discriminate, so the assertion above accepts "+
			"failures that have nothing to do with the parsing mode", dead)
	}
}

// isCheckoutRefusal reports whether err is the pool declining to hand out a
// connection because the checkout hook rejected every candidate.
//
// Drift is signalled to pgxpool as (false, nil) -- destroy this one and try
// another -- precisely so a drifted session self-heals. That carries no error
// of ours to match on, so what surfaces is the pool giving up after its bounded
// attempts. That wording is therefore the observable signature of a refusal,
// and the decoy above is what keeps this honest: if it ever starts matching an
// ordinary connection failure too, the cell says so instead of passing.
func isCheckoutRefusal(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "too many failed attempts acquiring connection") ||
		strings.Contains(msg, "never reported standard_conforming_strings")
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
