package rpc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yongjohnlee80/autodb/core/auth"
	"github.com/yongjohnlee80/autodb/core/exec"
	golibrpc "github.com/yongjohnlee80/golib/server/rpc"
)

// autodb wire error codes, alongside the transport's (-32601..-32001).
const (
	// CodeHandshakeRequired gates methods before a compatible sys.hello.
	CodeHandshakeRequired int64 = -32021
	// CodeProtocolMismatch refuses an incompatible client (re-provision).
	CodeProtocolMismatch int64 = -32020
	// CodeAuth carries credential/session failures (bad login, stale token,
	// locked store, policy refusals like weak passphrase or last admin).
	CodeAuth int64 = -32030
	// CodeDenied carries authorization denials (role/grant/IP).
	CodeDenied int64 = -32031
	// CodeStatementRejected carries the execution gate's refusals
	// (classification, WHERE-less guard, size cap).
	CodeStatementRejected int64 = -32032
)

// wireErr maps core errors onto the wire. The transport withholds any
// non-*Error text (deny-before-disclose), so this is the ONE place autodb
// decides which core messages are deliberately public: the auth/exec
// sentinel texts are user-facing by design and carry no internals. Anything
// unmapped stays server-side and reaches the peer as a generic internal
// error.
func wireErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, auth.ErrDenied):
		return &golibrpc.Error{Code: CodeDenied, Message: auth.ErrDenied.Error()}
	case errors.Is(err, auth.ErrBadCredentials),
		errors.Is(err, auth.ErrTokenInvalid),
		errors.Is(err, auth.ErrLocked),
		errors.Is(err, auth.ErrWeakPassphrase),
		errors.Is(err, auth.ErrBootstrapDone),
		errors.Is(err, auth.ErrLastAdmin),
		errors.Is(err, auth.ErrNoKeyslot):
		return &golibrpc.Error{Code: CodeAuth, Message: err.Error()}
	case errors.Is(err, exec.ErrEmptyStatement),
		errors.Is(err, exec.ErrMultiStatement),
		errors.Is(err, exec.ErrStatementUnsupported),
		errors.Is(err, exec.ErrMalformedStatement),
		errors.Is(err, exec.ErrNoWhere),
		errors.Is(err, exec.ErrScriptTooLarge):
		return &golibrpc.Error{Code: CodeStatementRejected, Message: err.Error()}
	}
	return err // transport logs it; peer sees a generic internal error
}

// --- positional argument decoding (msgpack-RPC params are arrays) ---

func argStr(p []any, i int, name string) (string, error) {
	if i >= len(p) {
		return "", &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
			Message: fmt.Sprintf("missing argument %d (%s)", i, name)}
	}
	s, ok := p[i].(string)
	if !ok {
		return "", &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
			Message: fmt.Sprintf("argument %d (%s): want string, got %T", i, name, p[i])}
	}
	return s, nil
}

func argInt(p []any, i int, name string) (int64, error) {
	if i >= len(p) {
		return 0, &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
			Message: fmt.Sprintf("missing argument %d (%s)", i, name)}
	}
	n, ok := p[i].(int64)
	if !ok {
		return 0, &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
			Message: fmt.Sprintf("argument %d (%s): want int, got %T", i, name, p[i])}
	}
	return n, nil
}

func argBool(p []any, i int, name string) (bool, error) {
	if i >= len(p) {
		return false, &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
			Message: fmt.Sprintf("missing argument %d (%s)", i, name)}
	}
	b, ok := p[i].(bool)
	if !ok {
		return false, &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
			Message: fmt.Sprintf("argument %d (%s): want bool, got %T", i, name, p[i])}
	}
	return b, nil
}

func identMap(id auth.Identity) map[string]any {
	return map[string]any{"id": id.UserID(), "name": id.Name(), "role": id.Role()}
}

// register wires the v1 method surface (ADR-0056 §2). Every handler is a
// mechanical projection: decode positional args, call the core with the
// peer IP threaded through, map the result/error. No business logic.
func (s *Server) register() {
	s.rpc.Handle("sys.hello", s.helloHandler)

	// --- auth: sessions & bootstrap ---
	s.rpc.Handle("auth.needs_bootstrap", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		need, err := s.auth.NeedsBootstrap(ctx)
		return need, wireErr(err)
	})
	s.rpc.Handle("auth.bootstrap", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		name, err := argStr(req.Params, 0, "name")
		if err != nil {
			return nil, err
		}
		pass, err := argStr(req.Params, 1, "passphrase")
		if err != nil {
			return nil, err
		}
		token, id, err := s.auth.Bootstrap(ctx, name, pass, peerIP(req))
		if err != nil {
			return nil, wireErr(err)
		}
		return map[string]any{"token": token, "user": identMap(id)}, nil
	})
	s.rpc.Handle("auth.login", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		name, err := argStr(req.Params, 0, "name")
		if err != nil {
			return nil, err
		}
		pass, err := argStr(req.Params, 1, "passphrase")
		if err != nil {
			return nil, err
		}
		token, id, err := s.auth.Login(ctx, name, pass, peerIP(req))
		if err != nil {
			return nil, wireErr(err)
		}
		return map[string]any{"token": token, "user": identMap(id)}, nil
	})
	s.rpc.Handle("auth.logout", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		return nil, wireErr(s.auth.Logout(ctx, token, peerIP(req)))
	})
	s.rpc.Handle("auth.whoami", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		id, err := s.auth.ValidateToken(ctx, token)
		if err != nil {
			return nil, wireErr(err)
		}
		return identMap(id), nil
	})

	// --- auth: user management (admin, token-first) ---
	s.rpc.Handle("auth.user_create", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		name, err := argStr(req.Params, 1, "name")
		if err != nil {
			return nil, err
		}
		pass, err := argStr(req.Params, 2, "passphrase")
		if err != nil {
			return nil, err
		}
		role, err := argStr(req.Params, 3, "role")
		if err != nil {
			return nil, err
		}
		id, err := s.auth.CreateUser(ctx, token, name, pass, role, peerIP(req))
		if err != nil {
			return nil, wireErr(err)
		}
		return id, nil
	})
	s.rpc.Handle("auth.user_role", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		role, err := argStr(req.Params, 2, "role")
		if err != nil {
			return nil, err
		}
		return nil, wireErr(s.auth.SetUserRole(ctx, token, userID, role, peerIP(req)))
	})
	s.rpc.Handle("auth.user_disable", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		disabled, err := argBool(req.Params, 2, "disabled")
		if err != nil {
			return nil, err
		}
		return nil, wireErr(s.auth.SetUserDisabled(ctx, token, userID, disabled, peerIP(req)))
	})
	s.rpc.Handle("auth.user_remove", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		return nil, wireErr(s.auth.RemoveUser(ctx, token, userID, peerIP(req)))
	})
	s.rpc.Handle("auth.passphrase_change", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		oldPass, err := argStr(req.Params, 1, "old_passphrase")
		if err != nil {
			return nil, err
		}
		newPass, err := argStr(req.Params, 2, "new_passphrase")
		if err != nil {
			return nil, err
		}
		return nil, wireErr(s.auth.ChangePassphrase(ctx, token, oldPass, newPass, peerIP(req)))
	})
	s.rpc.Handle("auth.passphrase_reset", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		newPass, err := argStr(req.Params, 2, "new_passphrase")
		if err != nil {
			return nil, err
		}
		return nil, wireErr(s.auth.ResetPassphrase(ctx, token, userID, newPass, peerIP(req)))
	})

	// --- auth: grants & allowlist (admin, token-first) ---
	s.rpc.Handle("auth.grant_add", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 2, "conn_id")
		if err != nil {
			return nil, err
		}
		role, err := argStr(req.Params, 3, "role")
		if err != nil {
			return nil, err
		}
		return nil, wireErr(s.auth.AddGrant(ctx, token, userID, connID, role, peerIP(req)))
	})
	s.rpc.Handle("auth.grant_remove", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 2, "conn_id")
		if err != nil {
			return nil, err
		}
		return nil, wireErr(s.auth.RemoveGrant(ctx, token, userID, connID, peerIP(req)))
	})
	s.rpc.Handle("auth.allowlist_add", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		cidr, err := argStr(req.Params, 1, "cidr")
		if err != nil {
			return nil, err
		}
		note, err := argStr(req.Params, 2, "note")
		if err != nil {
			return nil, err
		}
		return nil, wireErr(s.auth.AddAllowedIP(ctx, token, cidr, note, peerIP(req)))
	})
	s.rpc.Handle("auth.allowlist_remove", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		cidr, err := argStr(req.Params, 1, "cidr")
		if err != nil {
			return nil, err
		}
		return nil, wireErr(s.auth.RemoveAllowedIP(ctx, token, cidr, peerIP(req)))
	})

	// --- conn: connection management ---
	s.rpc.Handle("conn.create", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		name, err := argStr(req.Params, 1, "name")
		if err != nil {
			return nil, err
		}
		engine, err := argStr(req.Params, 2, "engine")
		if err != nil {
			return nil, err
		}
		dsn, err := argStr(req.Params, 3, "dsn")
		if err != nil {
			return nil, err
		}
		id, err := s.eng.CreateConnection(ctx, token, name, engine, dsn, peerIP(req))
		if err != nil {
			return nil, wireErr(err)
		}
		return id, nil
	})
	s.rpc.Handle("conn.list", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		conns, err := s.eng.ListConnections(ctx, token)
		if err != nil {
			return nil, wireErr(err)
		}
		out := make([]any, 0, len(conns))
		for _, c := range conns {
			out = append(out, map[string]any{
				"id": c.ID, "name": c.Name, "engine": c.Engine,
				"created_by": c.CreatedBy, "created_at": c.CreatedAt,
				"updated_at": c.UpdatedAt,
			})
		}
		return out, nil
	})
	s.rpc.Handle("conn.delete", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 1, "conn_id")
		if err != nil {
			return nil, err
		}
		return nil, wireErr(s.eng.DeleteConnection(ctx, token, connID, peerIP(req)))
	})
	s.rpc.Handle("conn.test", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 1, "conn_id")
		if err != nil {
			return nil, err
		}
		return nil, wireErr(s.eng.TestConnection(ctx, token, connID, peerIP(req)))
	})

	// --- exec ---
	s.rpc.Handle("exec.run", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 1, "conn_id")
		if err != nil {
			return nil, err
		}
		sqlText, err := argStr(req.Params, 2, "sql")
		if err != nil {
			return nil, err
		}
		res, err := s.eng.Execute(ctx, token, connID, sqlText, peerIP(req))
		if err != nil {
			return nil, wireErr(err)
		}
		return resultMap(res), nil
	})
}

// resultMap projects exec.Result onto the wire (ADR-0056 §2 exec.run shape).
// Row cells are normalized into the msgpack vocabulary — drivers hand back
// types the wire doesn't carry (pgx timestamps as time.Time), and one
// exotic cell must not fail the whole result page.
func resultMap(res *exec.Result) map[string]any {
	cols := make([]any, 0, len(res.Columns))
	for _, c := range res.Columns {
		cols = append(cols, c)
	}
	rows := make([]any, 0, len(res.Rows))
	for _, r := range res.Rows {
		row := make([]any, len(r))
		for i, v := range r {
			row[i] = wireVal(v)
		}
		rows = append(rows, row)
	}
	return map[string]any{
		"verb":        res.Verb,
		"class":       string(res.Class),
		"columns":     cols,
		"rows":        rows,
		"more":        res.More,
		"affected":    res.Affected,
		"duration_ms": res.Duration.Milliseconds(),
	}
}

// wireVal normalizes one result cell. Types the codec carries pass through;
// time.Time becomes RFC3339Nano; anything else stringifies — the FEs are
// display surfaces, and a lossy-but-visible cell beats a failed page.
func wireVal(v any) any {
	switch x := v.(type) {
	case nil, bool, int64, float64, string, []byte,
		int, int8, int16, int32, uint, uint8, uint16, uint32, uint64, float32:
		return x
	case time.Time:
		return x.Format(time.RFC3339Nano)
	default:
		return fmt.Sprintf("%v", x)
	}
}
