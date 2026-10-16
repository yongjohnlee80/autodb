// Package autodb is the plugin's side of AutoDB: finding or installing the binary, starting its
// server, and a thin msgpack-RPC client for the handful of verbs the plugin calls. It imports no
// AutoDB UI package; the client is golib's server/rpc, as AutoDB's own probe dials.
package autodb

import (
	"context"
	"errors"
	"fmt"
	"strings"

	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	"github.com/yongjohnlee80/golib/server/rpc/msgpackrpc"
)

// Protocol is the AutoDB wire protocol this plugin speaks: the one that has the session cursor
// verbs. A server of another protocol refuses the hello.
const Protocol int64 = 14

// The AutoDB error codes the plugin acts on. Each is AutoDB's own number on the wire.
const (
	// CodeProtocolMismatch is a hello from another protocol.
	CodeProtocolMismatch int64 = -32020
	// CodeAuth is a credential failure: a bad login, a token no longer valid, or a store locked
	// again by a restarted server.
	CodeAuth int64 = -32030
	// CodeSessionNotFound is a session that is gone: closed, reaped when idle, or never the
	// caller's.
	CodeSessionNotFound int64 = -32040
	// CodeTxState is a request wrong for the session's transaction: among others, a cursor that
	// has ended.
	CodeTxState int64 = -32043
)

// Code is err's AutoDB error code, and whether err is an answer from the server at all. An error
// that is not one is the transport's: the connection is gone.
func Code(err error) (int64, bool) {
	var re *golibrpc.Error
	if errors.As(err, &re) {
		return re.Code, true
	}
	return 0, false
}

// IsCode reports whether err is the server's answer with code.
func IsCode(err error, code int64) bool {
	c, ok := Code(err)
	return ok && c == code
}

// Message is err as the card says it: the server's own message for an answer, the error's text
// otherwise.
func Message(err error) string {
	var re *golibrpc.Error
	if errors.As(err, &re) {
		return re.Message
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// Hello is what the server said of itself.
type Hello struct {
	Version  string
	Instance string
	StoreID  string
}

// Client is one connection to an AutoDB server, past a compatible hello.
type Client struct {
	rpc   *golibrpc.Client
	Hello Hello
	// Addr is where it was reached, for the card.
	Addr string
}

// Dial connects to the server at network/addr and says hello at Protocol. A server of another
// protocol is refused with its own words.
func Dial(ctx context.Context, network, addr, clientVersion string) (*Client, error) {
	c, err := golibrpc.Dial(ctx, addr, msgpackrpc.New(nil), golibrpc.ClientNetwork(network))
	if err != nil {
		return nil, &DialError{Addr: addr, Err: err}
	}
	res, err := c.Call(ctx, "sys.hello", map[string]any{
		"protocol": Protocol, "name": "autodoc-autodb", "version": clientVersion,
	})
	if err != nil {
		_ = c.Close()
		if IsCode(err, CodeProtocolMismatch) {
			return nil, &MismatchError{Addr: addr, Message: Message(err)}
		}
		return nil, fmt.Errorf("hello: %w", err)
	}
	m, _ := res.(map[string]any)
	h := Hello{}
	h.Version, _ = m["version"].(string)
	h.Instance, _ = m["instance"].(string)
	h.StoreID, _ = m["store_id"].(string)
	return &Client{rpc: c, Hello: h, Addr: addr}, nil
}

// DialError is nothing answering at the address: no server is listening there.
type DialError struct {
	Addr string
	Err  error
}

func (e *DialError) Error() string { return fmt.Sprintf("nothing answers at %s: %v", e.Addr, e.Err) }

func (e *DialError) Unwrap() error { return e.Err }

// MismatchError is a server that speaks another protocol.
type MismatchError struct{ Addr, Message string }

func (e *MismatchError) Error() string {
	return fmt.Sprintf("the AutoDB at %s %s; this plugin speaks protocol %d: install the AutoDB release that matches it",
		e.Addr, strings.TrimPrefix(e.Message, "protocol mismatch: "), Protocol)
}

// Close ends the connection.
func (c *Client) Close() error { return c.rpc.Close() }

// Done is closed when the connection has ended.
func (c *Client) Done() <-chan struct{} { return c.rpc.Done() }

func (c *Client) call(ctx context.Context, method string, params ...any) (any, error) {
	return c.rpc.Call(ctx, method, params...)
}

// NeedsBootstrap reports whether the store has no user yet.
func (c *Client) NeedsBootstrap(ctx context.Context) (bool, error) {
	res, err := c.call(ctx, "auth.needs_bootstrap")
	if err != nil {
		return false, err
	}
	b, _ := res.(bool)
	return b, nil
}

// Login signs in and returns the session's token.
func (c *Client) Login(ctx context.Context, name, passphrase string) (token string, err error) {
	res, err := c.call(ctx, "auth.login", name, passphrase)
	if err != nil {
		return "", err
	}
	m, _ := res.(map[string]any)
	token, _ = m["token"].(string)
	if token == "" {
		return "", errors.New("auth.login answered without a token")
	}
	return token, nil
}

// Logout ends the token's session.
func (c *Client) Logout(ctx context.Context, token string) error {
	_, err := c.call(ctx, "auth.logout", token)
	return err
}

// Connection is one of AutoDB's connections, as the card lists it.
type Connection struct {
	ID     int64
	Name   string
	Engine string
	// Workspace is the workspace conn.list names the connection under; "" when it names none.
	Workspace string
}

// Connections are the connections the token may see.
func (c *Client) Connections(ctx context.Context, token string) ([]Connection, error) {
	res, err := c.call(ctx, "conn.list", token)
	if err != nil {
		return nil, err
	}
	list, _ := res.([]any)
	out := make([]Connection, 0, len(list))
	for _, v := range list {
		m, _ := v.(map[string]any)
		id, ok := intOf(m["id"])
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		eng, _ := m["engine"].(string)
		ws, _ := m["workspace"].(string)
		out = append(out, Connection{ID: id, Name: name, Engine: eng, Workspace: ws})
	}
	return out, nil
}

// Schemas are the schemas of connection conn's database (schema.schemas).
func (c *Client) Schemas(ctx context.Context, token string, conn int64) ([]string, error) {
	res, err := c.call(ctx, "schema.schemas", token, conn)
	if err != nil {
		return nil, err
	}
	list, _ := res.([]any)
	out := make([]string, 0, len(list))
	for _, v := range list {
		if n, ok := v.(string); ok {
			out = append(out, n)
		}
	}
	return out, nil
}

// Table is one relation of a schema, as schema.tables lists it. Quoted is its name as the server
// quotes it, schema and all: a trusted identifier to splice into SQL.
type Table struct {
	Schema, Name, Kind, Quoted string
}

// Tables are schema's tables and views (schema.tables).
func (c *Client) Tables(ctx context.Context, token string, conn int64, schema string) ([]Table, error) {
	res, err := c.call(ctx, "schema.tables", token, conn, schema)
	if err != nil {
		return nil, err
	}
	list, _ := res.([]any)
	out := make([]Table, 0, len(list))
	for _, v := range list {
		m, _ := v.(map[string]any)
		t := Table{}
		t.Schema, _ = m["schema"].(string)
		t.Name, _ = m["name"].(string)
		t.Kind, _ = m["kind"].(string)
		t.Quoted, _ = m["quoted"].(string)
		out = append(out, t)
	}
	return out, nil
}

// Column is one column of a relation, as schema.columns lists it. Quoted is its name as the server
// quotes it, "" from a server that does not say.
type Column struct {
	Name, Type, Quoted string
	PK, Nullable       bool
}

// Columns are schema.table's columns in order (schema.columns).
func (c *Client) Columns(ctx context.Context, token string, conn int64, schema, table string) ([]Column, error) {
	res, err := c.call(ctx, "schema.columns", token, conn, schema, table)
	if err != nil {
		return nil, err
	}
	list, _ := res.([]any)
	out := make([]Column, 0, len(list))
	for _, v := range list {
		m, _ := v.(map[string]any)
		col := Column{}
		col.Name, _ = m["name"].(string)
		col.Type, _ = m["type"].(string)
		col.Quoted, _ = m["quoted"].(string)
		col.PK, _ = m["pk"].(bool)
		col.Nullable, _ = m["nullable"].(bool)
		out = append(out, col)
	}
	return out, nil
}

// OpenSession opens a session on connection conn.
func (c *Client) OpenSession(ctx context.Context, token string, conn int64) (string, error) {
	res, err := c.call(ctx, "exec.session_open", token, conn)
	if err != nil {
		return "", err
	}
	m, _ := res.(map[string]any)
	sid, _ := m["session_id"].(string)
	if sid == "" {
		return "", errors.New("exec.session_open answered without a session")
	}
	return sid, nil
}

// CloseSession closes session sid.
func (c *Client) CloseSession(ctx context.Context, token, sid string) error {
	_, err := c.call(ctx, "exec.session_close", token, sid)
	return err
}

// Result is a statement's result as the wire carries it.
type Result struct {
	Verb       string
	Columns    []string
	Rows       [][]any
	More       bool
	Affected   int64
	DurationMS int64
	// Cursor is the cursor serving the rest of the rows; "" when the result is whole.
	Cursor string
	// Statements is how many statements a script ran (exec.run_script); 0 otherwise.
	Statements int64
}

// Query runs sql on session sid: a read on PostgreSQL opens a cursor and answers its first page
// of up to page rows (0 is the server's own page size); anything else answers whole.
func (c *Client) Query(ctx context.Context, token, sid, sql string, page int) (*Result, error) {
	res, err := c.call(ctx, "exec.session_query", token, sid, sql, int64(page))
	if err != nil {
		return nil, err
	}
	return readResult(res)
}

// Fetch is up to n more rows of cursor cur. More false: the cursor has ended.
func (c *Client) Fetch(ctx context.Context, token, sid, cur string, n int) (*Result, error) {
	res, err := c.call(ctx, "exec.session_fetch", token, sid, cur, int64(n))
	if err != nil {
		return nil, err
	}
	return readResult(res)
}

// CloseCursor closes cursor cur.
func (c *Client) CloseCursor(ctx context.Context, token, sid, cur string) error {
	_, err := c.call(ctx, "exec.session_cursor_close", token, sid, cur)
	return err
}

// RunScript runs a whole script on connection conn, the server splitting it; the result is the
// last statement's, its rows capped.
func (c *Client) RunScript(ctx context.Context, token string, conn int64, sql string) (*Result, error) {
	res, err := c.call(ctx, "exec.run_script", token, conn, sql)
	if err != nil {
		return nil, err
	}
	m, _ := res.(map[string]any)
	n, _ := intOf(m["statements"])
	out := &Result{}
	if r, ok := m["result"]; ok && r != nil {
		if out, err = readResult(r); err != nil {
			return nil, err
		}
	}
	out.Statements = n
	return out, nil
}

func readResult(v any) (*Result, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("a result that is not a map (%T)", v)
	}
	r := &Result{}
	r.Verb, _ = m["verb"].(string)
	r.More, _ = m["more"].(bool)
	r.Affected, _ = intOf(m["affected"])
	r.DurationMS, _ = intOf(m["duration_ms"])
	r.Cursor, _ = m["cursor"].(string)
	cols, _ := m["columns"].([]any)
	for _, c := range cols {
		s, _ := c.(string)
		r.Columns = append(r.Columns, s)
	}
	rows, _ := m["rows"].([]any)
	for _, row := range rows {
		cells, _ := row.([]any)
		r.Rows = append(r.Rows, cells)
	}
	return r, nil
}

// intOf is a whole number as the decoder gives it.
func intOf(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case uint64:
		return int64(n), n <= 1<<62
	case int:
		return int64(n), true
	}
	return 0, false
}
