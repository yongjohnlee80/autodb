package exec

import (
	"context"
	"errors"
	"testing"

	"github.com/yongjohnlee80/autodb/core/engine"
	"github.com/yongjohnlee80/autodb/core/meta"
	"github.com/yongjohnlee80/golib/dao"
	"github.com/yongjohnlee80/golib/dao/postgres"
)

func TestValidateDSN(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		engine engine.Name
		dsn    string
		ok     bool
	}{
		// MySQL — parsed with go-sql-driver's own parser.
		{"mysql clean", "mysql", "u:p@tcp(h:3306)/db?parseTime=true", true},
		{"mysql multiStatements", "mysql", "u:p@tcp(h:3306)/db?multiStatements=true", false},
		{"mysql interpolateParams", "mysql", "u:p@tcp(h:3306)/db?interpolateParams=true", false},
		{"mysql sql_mode param", "mysql", "u:p@tcp(h:3306)/db?sql_mode=NO_BACKSLASH_ESCAPES", false},
		{"mysql sql_mode mixed case", "mysql", "u:p@tcp(h:3306)/db?SQL_MODE=%27ANSI%27", false},
		{"mysql autocommit off", "mysql", "u:p@tcp(h:3306)/db?autocommit=0", false},
		{"mysql init_command", "mysql", "u:p@tcp(h:3306)/db?init_command=SET%20sql_mode%3DANSI", false},
		{"mysql malformed", "mysql", "not a dsn at all ::", false},
		// A password merely CONTAINING a banned word must not false-positive
		// (the old substring check would have rejected this).
		{"mysql banned word in password", "mysql", "u:sql_mode=x@tcp(h:3306)/db", true},

		// Postgres — parsed with pgxpool's own parser.
		{"pg clean", "postgres", "postgres://u:p@h:5432/db?sslmode=disable", true},
		{"pg scs runtime param", "postgres", "postgres://h/db?standard_conforming_strings=off", false},
		{"pg scs via options -c", "postgres", "postgres://h/db?options=-c%20standard_conforming_strings%3Doff", false},
		{"pg scs via options --flag", "postgres", "postgres://h/db?options=--standard_conforming_strings%3Doff", false},
		{"pg unrelated options ok", "postgres", "postgres://h/db?options=-c%20search_path%3Dapp", true},
		// The simple protocol runs every statement one Query message carries,
		// so it is refused in both DSN forms; every extended-protocol mode
		// pgx accepts is not.
		{"pg simple_protocol", "postgres", "postgres://h/db?default_query_exec_mode=simple_protocol", false},
		{"pg simple_protocol keyword form", "postgres", "host=h dbname=db default_query_exec_mode=simple_protocol", false},
		{"pg exec mode cache_statement", "postgres", "postgres://h/db?default_query_exec_mode=cache_statement", true},
		{"pg exec mode cache_describe", "postgres", "postgres://h/db?default_query_exec_mode=cache_describe", true},
		{"pg exec mode describe_exec", "postgres", "postgres://h/db?default_query_exec_mode=describe_exec", true},
		{"pg exec mode exec", "postgres", "postgres://h/db?default_query_exec_mode=exec", true},
		// Read from the parsed config, never the text: the words in a
		// password are not the mode.
		{"pg simple_protocol words in password", "postgres", "postgres://u:default_query_exec_mode%3Dsimple_protocol@h/db", true},
		{"pg malformed", "postgres", "://///", false},

		{"sqlite anything", "sqlite", "file:x?mode=memory", true},
		{"unknown engine", "oracle", "whatever", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateDSN(tc.engine, tc.dsn)
			if tc.ok && err != nil {
				t.Errorf("rejected valid DSN: %v", err)
			}
			if !tc.ok && err == nil {
				t.Errorf("accepted DSN that should be refused")
			}
		})
	}
}

// A STORED DSN IS JUDGED WHEN IT IS OPENED, NOT ONLY WHEN IT IS CREATED.
//
// A connection created before the simple protocol was refused still carries
// its DSN, sealed, and nothing re-validates rows at rest. What keeps it from
// serving a statement is that every open of a target runs the same validator
// creation does, before a driver is built. This drives a statement through
// Execute at such a row and requires the refusal to be the configuration fact
// it is — never a pool, never a dial.
func TestExecute_StoredSimpleProtocolDSNIsRefusedBeforeAnyDriver(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	enc, err := f.eng.auth.EncryptSecret([]byte("postgres://u:p@127.0.0.1:1/db?default_query_exec_mode=simple_protocol"), f.connID)
	if err != nil {
		t.Fatalf("sealing the DSN: %v", err)
	}
	if err := f.store.Connections.OnCtx(ctx).With(meta.ConnID, f.connID).
		Set(meta.ConnEngine, engine.Postgres.String()).Set(meta.ConnDSNEnc, enc).Update(); err != nil {
		t.Fatalf("storing the DSN: %v", err)
	}
	f.eng.closeTarget(f.connID)

	opens := 0
	orig := openPostgres
	t.Cleanup(func() { openPostgres = orig })
	openPostgres = func(_ context.Context, _, _ string, _ ...postgres.Option) (dao.DataConn, error) {
		opens++
		return nil, errors.New("the driver was reached")
	}

	_, err = f.eng.Execute(ctx, f.rootTok, f.connID, "SELECT 1", testIP)
	c, ok := ConfigFailureOf(err)
	if !ok {
		t.Fatalf("err = %v, want a ConfigFailure", err)
	}
	if c.Stage() != ConfigStageDSN || c.Detail() != DetailDSNUnusable {
		t.Errorf("stage/detail = %q/%q, want %q/%q", c.Stage(), c.Detail(), ConfigStageDSN, DetailDSNUnusable)
	}
	if opens != 0 {
		t.Errorf("a postgres driver was constructed %d time(s) for a DSN the validator refuses", opens)
	}
}
