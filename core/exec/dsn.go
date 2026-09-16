package exec

import (
	"context"
	"errors"
	"fmt"
	"github.com/yongjohnlee80/autodb/core/engine"
	"strings"

	drvmysql "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yongjohnlee80/golib/dao"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"
)

// The tokenizer models one dialect per engine. Two mechanisms keep the
// model synchronized with the target:
//
//  1. ValidateDSN parses the DSN with the ACTUAL driver parsers
//     (go-sql-driver's ParseDSN, pgxpool's ParseConfig — never substring
//     matching) and rejects options that change the grammar or statement
//     semantics.
//  2. Because dao.DataConn is a POOL, grammar is verified per PHYSICAL
//     session at execution time: every mysql/postgres statement runs inside
//     a transaction (one pinned session) whose mode is checked first — see
//     the session-grammar verifier and the engine's transactional run path. A one-time
//     pool probe cannot speak for later pool members or replacements.
//
// The long-term optimization is a golib per-connect hook (pgxpool
// AfterConnect; a connector seam for go-sql-driver) so the per-execution
// check can be dropped — tracked as golib upstream work.

// ValidateDSN rejects DSNs whose options would desynchronize the classifier
// from the target's actual grammar or change statement semantics.
func ValidateDSN(engineName engine.Name, dsn string) error {
	switch engineName {
	case engine.MySQL:
		cfg, err := drvmysql.ParseDSN(dsn)
		if err != nil {
			return fmt.Errorf("exec: invalid mysql DSN: %w", err)
		}
		if cfg.MultiStatements {
			return fmt.Errorf("exec: mysql DSN must not enable multiStatements: multi-statement mode defeats the one-statement-per-call gate")
		}
		if cfg.InterpolateParams {
			return fmt.Errorf("exec: mysql DSN must not enable interpolateParams: client-side interpolation rewrites the statement after classification")
		}
		for key, val := range cfg.Params {
			switch strings.ToLower(key) {
			case "sql_mode":
				return fmt.Errorf("exec: mysql DSN must not set sql_mode (%q): it can change string and identifier parsing", val)
			case "autocommit":
				if !strings.EqualFold(strings.Trim(val, "'\""), "1") && !strings.EqualFold(strings.Trim(val, "'\""), "true") && !strings.EqualFold(strings.Trim(val, "'\""), "on") {
					return fmt.Errorf("exec: mysql DSN must not disable autocommit: the engine's single-statement semantics assume it")
				}
			case "init_command":
				return fmt.Errorf("exec: mysql DSN must not set init_command")
			}
		}
		return nil

	case engine.Postgres:
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			return fmt.Errorf("exec: invalid postgres DSN: %w", err)
		}
		rp := cfg.ConnConfig.RuntimeParams
		for key, val := range rp {
			switch strings.ToLower(key) {
			case "standard_conforming_strings":
				return fmt.Errorf("exec: postgres DSN must not set standard_conforming_strings (%q): the classifier assumes the modern default", val)
			case "options":
				if optionsSetsParam(val, "standard_conforming_strings") {
					return fmt.Errorf("exec: postgres DSN options must not set standard_conforming_strings")
				}
			}
		}
		return nil

	case engine.SQLite:
		return nil
	default:
		return fmt.Errorf("exec: unsupported connection engine %q (postgres, mysql, sqlite)", engineName)
	}
}

// pgPrepareConnVerify returns a golib postgres.Option installing a pgxpool
// PrepareConn hook: the session's parsing mode is verified before EVERY
// acquisition — not only at establishment — so a session mutated after it
// was pooled (e.g. a verb-level read running
// `SELECT set_config('standard_conforming_strings','off',false)`, which the
// v1 reader contract permits) can never serve a later statement
// (raised in review). A drifted session returns (false, nil): pgxpool
// destroys it and retries on a fresh connection — self-healing — while a
// server whose DEFAULT is incompatible fails every fresh connection and
// surfaces pgxpool's bounded-attempts acquire error, fail-closed.
// Statements stay in plain autocommit, so transaction-prohibited DDL
// (VACUUM, CREATE DATABASE, CONCURRENTLY forms) remains executable.
// Existing PrepareConn/BeforeAcquire hooks are chained first.
func pgPrepareConnVerify() golibpg.Option {
	return func(cfg *pgxpool.Config) {
		prevPrepare := cfg.PrepareConn
		prevBefore := cfg.BeforeAcquire //nolint:staticcheck // chained for completeness
		cfg.PrepareConn = func(ctx context.Context, conn *pgx.Conn) (bool, error) {
			if prevPrepare != nil {
				ok, err := prevPrepare(ctx, conn)
				if !ok || err != nil {
					return ok, err
				}
			} else if prevBefore != nil && !prevBefore(ctx, conn) {
				return false, nil
			}
			// READ WHAT THE SERVER ALREADY TOLD US. DO NOT ASK.
			//
			// standard_conforming_strings is a GUC_REPORT parameter: PostgreSQL
			// sends a ParameterStatus at startup and again on EVERY change,
			// unprompted -- including a change made through
			// `SELECT set_config(...)`, which is the form the SET denylist
			// cannot intercept and the reason this check exists at all.
			// Measured: set_config to 'off' updates the reported value with no
			// query issued.
			//
			// Asking instead cost a round trip per checkout, and it is what
			// made autodb unusable through its own front door. pgx names a
			// cached statement `stmtcache_` + sha256(sql)[:24] -- a pure
			// function of the SQL TEXT. A wire session pins ONE target backend
			// and relays the client's Parse onto it, so when the client is also
			// pgx in its shipped default it computes THE SAME NAME for this
			// same text. autodb had already prepared it on that session, so the
			// client's very first Parse came back 42P05 "already exists",
			// deterministically, on a brand-new connection.
			//
			// The collision was never the disease. Issuing a statement to learn
			// something the protocol reports is: it put autodb's own objects in
			// the namespace it hands to clients. Reading the reported value
			// takes autodb out of that namespace entirely, and is strictly more
			// truthful -- it reflects every change the server announced rather
			// than the value at the moment we happened to ask.
			// The RULE lives in the dialect; only the TRANSPORT is here.
			// pgconn already holds every ParameterStatus the server sent, so
			// its lookup is handed over directly.
			v, ok := dialectFor(engine.Postgres).(ReportedGrammarVerifier)
			if !ok {
				return false, fmt.Errorf(
					"exec: the postgres dialect no longer verifies its parsing mode")
			}
			if verr := v.VerifyReportedGrammar(conn.PgConn().ParameterStatus); verr != nil {
				if errors.Is(verr, ErrGrammarDrifted) {
					// Drifted session: destroy it and let the pool retry on a
					// fresh one, which self-heals.
					return false, nil
				}
				// Unreadable, not drifted. Retrying would spin the pool's
				// bounded attempts against a peer that will never answer.
				return false, fmt.Errorf("exec: verifying the parsing mode at checkout: %w", verr)
			}
			return true, nil
		}
	}
}

// pgAfterReleaseReset returns a golib postgres.Option installing a pgxpool
// AfterRelease hook that runs THE SAME reset plan the session release gate runs
// (release_gate.go) on every ordinary connection the driver takes back, and
// destroys the connection when the reset cannot be proved.
//
// THE ORDINARY PATH LEAKS TOO, and only this closes it. The session release
// gate governs the backends autodb pins for a front-door session. An ordinary
// statement borrows from the SAME pool, and the driver returns it with no seam
// autodb owns — so a plain SELECT that calls a routine which sets a
// configuration parameter leaves that parameter on a connection the next
// ordinary statement, belonging to a different developer, is handed. Sanitation
// at session checkout protects the session and nobody else; ordinary→ordinary
// was, until this hook, an unguarded exchange of session state.
//
// RELEASE, NOT ACQUIRE, and the difference is not a preference. A connection
// cleaned on the way out is clean for the whole time it sits idle, so every
// borrower is protected by one run of the plan rather than each borrower paying
// for its own. Acquire-time sanitation would also run under the CALLER'S
// context: a cancelled request would skip the reset on the connection it just
// dirtied, which is precisely the case that most needs it. And pgxpool calls
// this hook only on a connection it is willing to reuse — a closed, busy or
// mid-transaction one is destroyed before the hook is reached — so the hook is
// asked exactly the question it can answer.
//
// THE COST IS REAL AND IS THE POINT. Every ordinary statement now pays the
// plan's round trips when its connection goes back. That buys the guarantee
// that nothing a statement leaves behind can reach the next borrower, and the
// alternative on offer was to keep the leak. pgxpool runs this hook on its own
// goroutine, so the statement's own latency is unchanged; what a saturated pool
// pays is the wait for the member to come back.
//
// Returning false destroys the connection, which is the same answer the release
// gate gives a reset it cannot prove: a discarded backend costs one reconnect,
// a wrongly pooled one hands a stranger's session state to the next person.
//
// Existing AfterRelease hooks are chained first, and a refusal from one of them
// stands — this hook may only ever be the stricter of the two.
func (e *Engine) pgAfterReleaseReset(connID int64) golibpg.Option {
	return func(cfg *pgxpool.Config) {
		prev := cfg.AfterRelease
		cfg.AfterRelease = func(conn *pgx.Conn) bool {
			if prev != nil && !prev(conn) {
				return false
			}
			v := e.runResetPlan(context.Background(), poolResetRunner{conn: conn})
			if v.pooled {
				return true
			}
			e.logf("connection %d: a pooled backend was destroyed rather than reused (%s)",
				connID, v.reason())
			return false
		}
	}
}

// poolResetRunner carries the reset plan over the connection pgxpool hands its
// release hook.
//
// It dispatches through the SIMPLE protocol rather than pgx's high-level query
// path, for the same reason the session path uses the simple-query face: the
// reset statements must not enter pgx's prepared-statement cache. A cached
// statement is named from a hash of its SQL text, a wire client later pinning
// this same connection computes the same name for the same text, and autodb's
// own objects in that namespace are what answered a client's first Parse with
// 42P05.
//
// THE ONE STEP THAT CANNOT GO STRAIGHT DOWN THE WIRE is the deallocation, and
// finding that out cost a live cell. pgx caches a prepared statement per
// connection; a bare DEALLOCATE ALL removes the server's copy and leaves pgx
// certain its own still exists, so the next ordinary statement with that text
// came back 26000 on a connection the pool considered healthy. The driver's own
// DeallocateAll does both halves, and the plan marks the step that needs it
// rather than this code matching on its text — a renamed or rewritten step must
// not silently lose the client-side half.
type poolResetRunner struct{ conn *pgx.Conn }

func (r poolResetRunner) runResetStatement(ctx context.Context, step resetStep) (byte, *pgconn.PgError, error) {
	pg := r.conn.PgConn()
	var err error
	if step.invalidatesDriverCache {
		err = r.conn.DeallocateAll(ctx)
	} else {
		_, err = pg.Exec(ctx, step.sql).ReadAll()
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			// The server refused the statement and the wire is intact. That is
			// protocol data, not a transport failure, and the verdict tells
			// them apart.
			return pg.TxStatus(), pgErr, nil
		}
		return 0, nil, err
	}
	return pg.TxStatus(), nil, nil
}

// optionsSetsParam is a BEST-EFFORT field matcher over a libpq-style options
// string ("-c name=value --name=value …") — it does NOT implement libpq's
// escaping grammar, so it exists only as a fast, clear failure at
// creation time. The authoritative check is live per-connection
// verification (pgPrepareConnVerify), which a smuggled setting cannot
// evade.
func optionsSetsParam(options, name string) bool {
	fields := strings.Fields(options)
	for i, f := range fields {
		switch {
		case f == "-c" && i+1 < len(fields):
			if strings.HasPrefix(strings.ToLower(fields[i+1]), name+"=") {
				return true
			}
		case strings.HasPrefix(strings.ToLower(f), "-c"+name+"="):
			return true
		case strings.HasPrefix(strings.ToLower(f), "--"+name+"="):
			return true
		}
	}
	return false
}

// scalarStringQ runs q (one text column, one row) on the given querier.
func scalarStringQ(ctx context.Context, querier dao.Querier, stmt string, args ...any) (string, error) {
	rows, err := querier.QueryContext(ctx, stmt, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if !rows.Next() {
		return "", rows.Err()
	}
	var v string
	if err := rows.Scan(&v); err != nil {
		return "", err
	}
	return v, nil
}

// TargetDBName returns the database name a DSN points at, using THE DRIVER'S
// OWN PARSER for the engine in question.
//
// Parsed, never pattern-matched: security-core-hardening R11's rule is that a
// gate reading a DSN must agree with the driver that will use it, and a
// substring reading of a connection string disagrees with every driver
// eventually. It is the same reason ValidateDSN above parses rather than
// greps, and this deliberately reuses those parsers rather than adding a
// second reading of the same string.
//
// The result is stored in connections.target_db, in PLAINTEXT. The database
// NAME is not a secret — the DSN's credentials are — and holding it as a
// column is what makes the front door's startup cross-check an indexed read
// instead of N DSN decryptions on the authentication path.
//
// THIS SWITCH IS THE ONE PLACE THAT GROWS PER ENGINE. mysql and bigquery are
// coming (Johno, 2026-09-05), and adding them is one case each — nothing else
// in the front door keys on an engine name, deliberately, because a gate that
// names engines has to be edited every time the set changes and is a proxy for
// the question actually being asked: is the target's database name knowable?
//
// An engine with no answer returns "" WITHOUT an error. That is not a failure:
// such a connection is reachable by its CONNECTION NAME, which is how every
// connection was reachable before this column existed. sqlite is the standing
// example — its "database" is a file path, not a name a client would type into
// a Database field. Empty therefore means "reachable by name only", never
// "misconfigured".
func TargetDBName(engineName engine.Name, dsn string) (string, error) {
	switch engineName {
	case engine.Postgres:
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			return "", fmt.Errorf("exec: invalid postgres DSN: %w", err)
		}
		return cfg.ConnConfig.Database, nil
	default:
		return "", nil
	}
}
