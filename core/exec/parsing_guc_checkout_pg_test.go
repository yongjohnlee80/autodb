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
	"fmt"
	"net/url"
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
// one the verifier exists for.
//
// ON A DATABASE OF ITS OWN. A database default reaches every new session on
// that database, not only this cell's: run against TEST_PGURL's database, it
// reached other packages' test binaries running at the same time, and a front
// door session opened in the window was refused with F0000. So the cell
// creates a scratch database, alters that, and drops it.
//
// WHAT THIS CELL DEFENDS. The fix deleted a probe and replaced a query with a
// read of reported state. Deleting the check entirely, or reading a parameter
// name that is never reported (so the value is always empty), would both leave
// autodb serving statements it cannot parse the same way the server does. This
// cell fails for all three.
func TestParsingGUC_ATargetReportingOffIsRefusedAtCheckout(t *testing.T) {
	shared := parsingGUCDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, shared)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())

	dbName := fmt.Sprintf("parsing_guc_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("creating the scratch database: %v", err)
	}
	// A failed drop fails the cell: a passing run that left a database behind
	// would hide the leak until the server ran out of room. Bounded on its own
	// context, because the cell's is spent or cancelled by the time this runs.
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		c, err := pgx.Connect(cctx, shared)
		if err != nil {
			t.Errorf("cleanup: reconnecting to drop %s: %v", dbName, err)
			return
		}
		defer c.Close(cctx)
		if _, err := c.Exec(cctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)"); err != nil {
			t.Errorf("cleanup: DROP DATABASE %s: %v", dbName, err)
		}
	})
	// TEST_PGURL is a URL, as the test docs give it. A dbname query parameter
	// would override the path and point the cell back at the shared database,
	// so it is removed; any other shape is refused rather than guessed at.
	u, err := url.Parse(shared)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("TEST_PGURL must be a postgres:// URL for this cell (parse error: %v)", err)
	}
	u.Path = "/" + dbName
	q := u.Query()
	q.Del("dbname")
	u.RawQuery = q.Encode()
	dsn := u.String()
	if _, err := admin.Exec(ctx, "ALTER DATABASE "+dbName+" SET standard_conforming_strings = off"); err != nil {
		t.Fatalf("ALTER DATABASE %s ... off: %v", dbName, err)
	}

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
		conn, oerr := openPostgres(ctx, "parsing-guc-off", dsn, pgPrepareConnVerify(0))
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
			"postgres://nobody:nope@127.0.0.1:1/nosuchdb?sslmode=disable", pgPrepareConnVerify(0))
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

// isCheckoutRefusal reports whether err is the checkout hook refusing a
// drifted connection.
//
// A pool backend is never reused, so a drifted connection is a fresh one: the
// server's own default is incompatible, and the hook fails it ONCE as a
// configuration fact (DetailGrammarUnproved) rather than asking pgxpool to
// redial. The decoy above keeps this honest: an ordinary connection failure
// must not match.
func isCheckoutRefusal(err error) bool {
	c, ok := ConfigFailureOf(err)
	return ok && c.Detail() == DetailGrammarUnproved
}

// AND A TARGET REPORTING `on` IS SERVED.
//
// The positive control for the cell above: without it, a verifier that refused
// EVERY connection would satisfy the refusal cell perfectly.
func TestParsingGUC_ATargetReportingOnIsServed(t *testing.T) {
	dsn := parsingGUCDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, err := openPostgres(ctx, "parsing-guc-on", dsn, pgPrepareConnVerify(0))
	if err != nil {
		t.Fatalf("a compatible target was refused at open: %v", err)
	}
	defer conn.Close()
	if _, err := scalarStringQ(ctx, conn, "SELECT 1"); err != nil {
		t.Fatalf("a compatible target was refused at checkout: %v", err)
	}
}
