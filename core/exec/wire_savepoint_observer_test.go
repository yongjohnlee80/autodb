package exec

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/yongjohnlee80/golib/dao"
	golibpg "github.com/yongjohnlee80/golib/dao/postgres"
)

type a25Tx struct{}

func (*a25Tx) QueryContext(context.Context, string, ...any) (dao.Rows, error)  { return nil, nil }
func (*a25Tx) ExecContext(context.Context, string, ...any) (dao.Result, error) { return nil, nil }
func (*a25Tx) Commit() error                                                   { return nil }
func (*a25Tx) Rollback() error                                                 { return nil }
func (*a25Tx) CommitContext(context.Context) error                             { return nil }
func (*a25Tx) RollbackContext(context.Context) error                           { return nil }

type a25Pinned struct{ tx dao.ContextTxConn }

func (*a25Pinned) Send(context.Context, golibpg.ExtendedOp) error { return nil }
func (*a25Pinned) Flush(context.Context) error                    { return nil }
func (*a25Pinned) Receive(context.Context) (golibpg.ExtendedMessage, error) {
	return golibpg.ExtendedMessage{}, nil
}
func (*a25Pinned) Sync(context.Context) (byte, error) { return 'I', nil }
func (p *a25Pinned) BeginSessionTx(context.Context, dao.TxOptions) (dao.ContextTxConn, error) {
	return p.tx, nil
}
func (*a25Pinned) Release(context.Context) error { return nil }
func (*a25Pinned) Discard()                      {}

type a25Simple struct{ sql []string }

func (s *a25Simple) SimpleQuery(_ context.Context, sqlText string,
	_ func(golibpg.ExtendedMessage) error) (byte, error) {
	s.sql = append(s.sql, sqlText)
	return 'I', nil
}

func TestWireSessionSQLObserver_AutodbUsesNoNamedSavepoint(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		staticStrings := collectStaticStrings(file)
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ingressCallName(call)
			if name != "ExecContext" && name != "QueryContext" && name != "SimpleQuery" && name != "sessionSimpleQuery" {
				return true
			}
			arg := emittedSQLArg(name, call.Args)
			if text, ok := staticString(arg, staticStrings); ok && isNamedSavepointControl(text) {
				t.Errorf("production source %s emits named savepoint control %q", path, text)
			}
			return true
		})
	}

	for _, sqlText := range []string{
		"SAVEPOINT internal_sp",
		"RELEASE SAVEPOINT internal_sp",
		"ROLLBACK TO SAVEPOINT internal_sp",
	} {
		if !isNamedSavepointControl(sqlText) {
			t.Fatalf("named savepoint control %q was not recognized", sqlText)
		}
	}

	type event struct {
		origin sessionSQLOrigin
		sql    string
	}
	var events []event
	e := &Engine{hookSessionSQL: func(origin sessionSQLOrigin, sqlText string) {
		events = append(events, event{origin: origin, sql: sqlText})
	}}
	ctx := context.Background()

	// All three forbidden spellings can occur in client text. Keep that origin
	// instead of blaming autodb merely because the bytes crossed its backend.
	clientSQL := `SELECT 'SAVEPOINT client_sp', 'RELEASE SAVEPOINT client_sp', 'ROLLBACK TO SAVEPOINT client_sp'`
	simple := &a25Simple{}
	if _, err := e.sessionSimpleQuery(ctx, simple, sessionSQLClient, clientSQL, func(golibpg.ExtendedMessage) error { return nil }); err != nil {
		t.Fatalf("relaying client SQL: %v", err)
	}
	if len(simple.sql) != 1 || simple.sql[0] != clientSQL {
		t.Fatalf("client relay = %q, want exact %q", simple.sql, clientSQL)
	}

	tx, err := e.beginProxiedTx(ctx, &a25Pinned{tx: &a25Tx{}}, dao.TxOptions{Access: dao.TxReadOnly})
	if err != nil {
		t.Fatalf("beginning proxied control transaction: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "SET LOCAL idle_in_transaction_session_timeout = '95s'"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.QueryContext(ctx, "SELECT txid_current()"); err != nil {
		t.Fatal(err)
	}
	if err := tx.CommitContext(ctx); err != nil {
		t.Fatal(err)
	}

	var internal []string
	seen := make(map[string]bool)
	for _, ev := range events {
		if ev.origin == sessionSQLAutodb {
			internal = append(internal, ev.sql)
			seen[ev.sql] = true
			if isNamedSavepointControl(ev.sql) {
				t.Fatalf("autodb emitted named savepoint control %q; internal operations: %q", ev.sql, internal)
			}
		}
	}
	if len(events) == 0 || events[0] != (event{origin: sessionSQLClient, sql: clientSQL}) {
		t.Fatalf("client relay was misattributed: events = %+v", events)
	}
	for _, want := range []string{
		"BEGIN",
		"SET LOCAL idle_in_transaction_session_timeout = '95s'",
		"SELECT txid_current()",
		"COMMIT",
	} {
		if !seen[want] {
			t.Errorf("autodb operation %q was not observed; got %q", want, internal)
		}
	}
}

func emittedSQLArg(name string, args []ast.Expr) ast.Expr {
	index := 1
	if name == "sessionSimpleQuery" {
		index = 3
	}
	if index >= len(args) {
		return nil
	}
	return args[index]
}

func collectStaticStrings(file *ast.File) map[string]string {
	values := make(map[string]string)
	for range 3 {
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.ValueSpec:
				for i, name := range node.Names {
					if i >= len(node.Values) {
						continue
					}
					if value, ok := staticString(node.Values[i], values); ok {
						values[name.Name] = value
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range node.Lhs {
					name, ok := lhs.(*ast.Ident)
					if !ok || i >= len(node.Rhs) {
						continue
					}
					if value, ok := staticString(node.Rhs[i], values); ok {
						values[name.Name] = value
					}
				}
			}
			return true
		})
	}
	return values
}

func staticString(expr ast.Expr, values map[string]string) (string, bool) {
	switch expr := expr.(type) {
	case *ast.BasicLit:
		if expr.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(expr.Value)
		return value, err == nil
	case *ast.Ident:
		value, ok := values[expr.Name]
		return value, ok
	case *ast.BinaryExpr:
		if expr.Op != token.ADD {
			return "", false
		}
		left, leftOK := staticString(expr.X, values)
		right, rightOK := staticString(expr.Y, values)
		return left + right, leftOK && rightOK
	default:
		return "", false
	}
}

func isNamedSavepointControl(sqlText string) bool {
	fields := strings.Fields(strings.ToUpper(sqlText))
	if len(fields) == 0 {
		return false
	}
	if fields[0] == "SAVEPOINT" || fields[0] == "RELEASE" {
		return true
	}
	return len(fields) > 1 && fields[0] == "ROLLBACK" && fields[1] == "TO"
}
