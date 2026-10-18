package autodb

import (
	"context"
	"errors"
	"fmt"
	"strings"

	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
	"github.com/yongjohnlee80/golib/server/rpc/msgpackrpc"
)

// Probe is sys.hello with no protocol: what the server at network/addr is, and its protocol. A
// probe admits nothing, so it answers a server of any protocol, and its connection is its own.
func Probe(ctx context.Context, network, addr string) (Hello, int, error) {
	c, err := golibrpc.Dial(ctx, addr, msgpackrpc.New(nil), golibrpc.ClientNetwork(network))
	if err != nil {
		return Hello{}, 0, &DialError{Addr: addr, Err: err}
	}
	defer c.Close()
	res, err := c.Call(ctx, "sys.hello", map[string]any{})
	if err != nil {
		return Hello{}, 0, fmt.Errorf("probe: %w", err)
	}
	m, _ := res.(map[string]any)
	proto, ok := intOf(m["protocol"])
	if server, _ := m["server"].(string); !ok || server != "autodb" {
		return Hello{}, 0, fmt.Errorf("what answers at %s is not AutoDB", addr)
	}
	return helloOf(m), int(proto), nil
}

// CodeDenied is AutoDB's authorization denial: a role the verb needs, the user has not.
const CodeDenied int64 = -32031

// IsUnknownMethod reports whether err is the server not knowing the verb: a release before it.
func IsUnknownMethod(err error) bool { return IsCode(err, golibrpc.CodeMethodNotFound) }

// Lifecycle is a connection to an AutoDB of any protocol, for restarting it: it says hello at the
// server's own protocol and offers only the verbs every protocol keeps unchanged (sys.hello,
// auth.login, auth.whoami, sys.restart_if_idle), so no product verb is ever sent to a server of
// another protocol.
type Lifecycle struct {
	rpc   *golibrpc.Client
	Hello Hello
}

// DialLifecycle connects to the server at network/addr, saying hello at protocol, its own.
func DialLifecycle(ctx context.Context, network, addr, clientVersion string, protocol int) (*Lifecycle, error) {
	c, err := golibrpc.Dial(ctx, addr, msgpackrpc.New(nil), golibrpc.ClientNetwork(network))
	if err != nil {
		return nil, &DialError{Addr: addr, Err: err}
	}
	res, err := c.Call(ctx, "sys.hello", map[string]any{
		"protocol": protocol, "name": "autodoc-autodb", "version": clientVersion,
	})
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("hello: %w", err)
	}
	m, _ := res.(map[string]any)
	return &Lifecycle{rpc: c, Hello: helloOf(m)}, nil
}

// Close ends the connection.
func (l *Lifecycle) Close() error { return l.rpc.Close() }

// Done is closed when the connection has ended: a server that stopped.
func (l *Lifecycle) Done() <-chan struct{} { return l.rpc.Done() }

// Login is auth.login.
func (l *Lifecycle) Login(ctx context.Context, name, passphrase string) (string, error) {
	return login(ctx, l.rpc.Call, name, passphrase)
}

// Whoami is auth.whoami.
func (l *Lifecycle) Whoami(ctx context.Context, token string) (Who, error) {
	return whoami(ctx, l.rpc.Call, token)
}

// RestartIfIdle is sys.restart_if_idle.
func (l *Lifecycle) RestartIfIdle(ctx context.Context, token string) (Restart, error) {
	return restartIfIdle(ctx, l.rpc.Call, token)
}

// RestartIfIdle is sys.restart_if_idle on the working connection: the server stops only if nothing
// runs (administrators only).
func (c *Client) RestartIfIdle(ctx context.Context, token string) (Restart, error) {
	return restartIfIdle(ctx, c.rpc.Call, token)
}

// caller is a connection's Call.
type caller func(ctx context.Context, method string, params ...any) (any, error)

// Restart is sys.restart_if_idle's answer: stopping, or what keeps the server busy.
type Restart struct {
	Stopping                                               bool
	InTransaction, Executing, WireSessions, RemoteSessions int64
}

// Busy says what keeps the server running.
func (r Restart) Busy() string {
	var parts []string
	for _, c := range []struct {
		n    int64
		what string
	}{
		{r.Executing, "running a statement"},
		{r.InTransaction, "in a transaction"},
		{r.WireSessions, "connected by another client"},
		{r.RemoteSessions, "signed in remotely"},
	} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.what))
		}
	}
	if len(parts) == 0 {
		return "busy"
	}
	return strings.Join(parts, ", ")
}

func restartIfIdle(ctx context.Context, call caller, token string) (Restart, error) {
	res, err := call(ctx, "sys.restart_if_idle", token)
	if err != nil {
		return Restart{}, err
	}
	m, _ := res.(map[string]any)
	r := Restart{}
	r.Stopping, _ = m["stopping"].(bool)
	busy, _ := m["busy"].(map[string]any)
	r.InTransaction, _ = intOf(busy["in_transaction"])
	r.Executing, _ = intOf(busy["executing"])
	r.WireSessions, _ = intOf(busy["wire_sessions"])
	r.RemoteSessions, _ = intOf(busy["remote_sessions"])
	return r, nil
}

func login(ctx context.Context, call caller, name, passphrase string) (string, error) {
	res, err := call(ctx, "auth.login", name, passphrase)
	if err != nil {
		return "", err
	}
	m, _ := res.(map[string]any)
	token, _ := m["token"].(string)
	if token == "" {
		return "", errors.New("auth.login answered without a token")
	}
	return token, nil
}

func whoami(ctx context.Context, call caller, token string) (Who, error) {
	res, err := call(ctx, "auth.whoami", token)
	if err != nil {
		return Who{}, err
	}
	m, _ := res.(map[string]any)
	w := Who{}
	w.ID, _ = intOf(m["id"])
	w.Name, _ = m["name"].(string)
	w.Role, _ = m["role"].(string)
	w.StoreUnlocked, w.Known = m["store_unlocked"].(bool)
	return w, nil
}
