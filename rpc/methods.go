package rpc

import (
	"context"
	"encoding"
	"errors"
	"fmt"
	"reflect"
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

// publicErrs is the whole disclosure allowlist: core sentinels whose
// CONSTANT text is deliberately public, each with its wire code. Order
// matters only in that ErrDenied precedes the CodeAuth family.
var publicErrs = []struct {
	sentinel error
	code     int64
}{
	{auth.ErrDenied, CodeDenied},
	{auth.ErrBadCredentials, CodeAuth},
	{auth.ErrTokenInvalid, CodeAuth},
	{auth.ErrLocked, CodeAuth},
	{auth.ErrWeakPassphrase, CodeAuth},
	{auth.ErrBootstrapDone, CodeAuth},
	{auth.ErrLastAdmin, CodeAuth},
	{auth.ErrNoKeyslot, CodeAuth},
	{exec.ErrEmptyStatement, CodeStatementRejected},
	{exec.ErrMultiStatement, CodeStatementRejected},
	{exec.ErrStatementUnsupported, CodeStatementRejected},
	{exec.ErrMalformedStatement, CodeStatementRejected},
	{exec.ErrNoWhere, CodeStatementRejected},
	{exec.ErrScriptTooLarge, CodeStatementRejected},
	// Workspace not-found is admin-only reachable (Manage authz runs
	// BEFORE the lookup, so R13 ordering holds) and carries no internals.
	{exec.ErrWorkspaceNotFound, golibrpc.CodeInvalidParams},
}

// wireErr maps core errors onto the wire. The transport withholds any
// non-*Error text (deny-before-disclose), so this is the ONE place autodb
// decides what is deliberately public — and it publishes the MATCHED
// SENTINEL's constant text, never err.Error(): a future wrapper adding
// context ("user 42 from 10.0.0.9: %w") would otherwise export its whole
// contextual string across the disclosure boundary. Anything unmapped
// stays server-side and reaches the peer as a generic internal error.
func wireErr(err error) error {
	if err == nil {
		return nil
	}
	for _, pe := range publicErrs {
		if errors.Is(err, pe.sentinel) {
			return &golibrpc.Error{Code: pe.code, Message: pe.sentinel.Error()}
		}
	}
	return err // transport logs it; peer sees a generic internal error
}

// --- positional argument decoding (msgpack-RPC params are arrays) ---

// exactArgs enforces exact positional arity: trailing extras are an invalid
// call, not silently ignored input.
func exactArgs(p []any, n int) error {
	if len(p) != n {
		return &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
			Message: fmt.Sprintf("want %d argument(s), got %d", n, len(p))}
	}
	return nil
}

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
	s.registerM6()

	// --- auth: sessions & bootstrap ---
	s.rpc.Handle("auth.needs_bootstrap", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 0); err != nil {
			return nil, err
		}
		need, err := s.auth.NeedsBootstrap(ctx)
		return need, wireErr(err)
	})
	s.rpc.Handle("auth.bootstrap", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
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
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
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
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		return nil, wireErr(s.auth.Logout(ctx, token, peerIP(req)))
	})
	s.rpc.Handle("auth.whoami", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
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
		if err := exactArgs(req.Params, 4); err != nil {
			return nil, err
		}
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
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
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
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
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
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
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
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
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
	// exec.run_script runs a multi-statement buffer sequentially and
	// returns the LAST statement's result. The core splits with the
	// classifier's own lexer and runs each statement through the normal
	// guarded path — the wire adds nothing.
	s.rpc.Handle("exec.run_script", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 1, "connection_id")
		if err != nil {
			return nil, err
		}
		sqlText, err := argStr(req.Params, 2, "sql")
		if err != nil {
			return nil, err
		}
		out, xerr := s.eng.ExecuteScript(ctx, token, connID, sqlText, peerIP(req))
		if xerr != nil {
			return nil, wireErr(xerr)
		}
		reply := map[string]any{"statements": int64(out.Statements)}
		if out.Last != nil {
			reply["result"] = resultMap(out.Last)
		}
		return reply, nil
	})

	// history.list is the script-history read side (Objective 5/20). The
	// CORE decides what the caller may see (admins everything, everyone
	// else their own executions) — the wire just projects it.
	s.rpc.Handle("history.list", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		limit, err := argInt(req.Params, 1, "limit")
		if err != nil {
			return nil, err
		}
		rows, herr := s.eng.ListHistory(ctx, token, int(limit))
		if herr != nil {
			return nil, wireErr(herr)
		}
		out := make([]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, map[string]any{
				"id": r.ID, "user_id": r.UserID, "user": r.User,
				"connection_id": r.ConnID, "connection": r.Conn, "ip": r.IP,
				"script": r.Script, "started_at": r.StartedAt.Format(time.RFC3339),
				"duration_ms": r.Duration.Milliseconds(), "row_count": r.RowCount,
				"status": r.Status, "error": r.Error,
			})
		}
		return out, nil
	})

	// sys.shutdown drains this server (ADR-0056 §3: the shared server
	// outlives its frontends, so restarting it needs an authorized
	// remote path — a rebuilt binary otherwise keeps serving from the
	// old process). Admin-only, audited BEFORE the effect (R6).
	s.rpc.Handle("sys.shutdown", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		ident, aerr := s.auth.RequireAdmin(ctx, token)
		if aerr != nil {
			return nil, wireErr(aerr)
		}
		if err := s.auth.Audit(ctx, ident.UserID(), peerIP(req),
			"server_shutdown", "requested over rpc"); err != nil {
			return nil, wireErr(err) // an unaudited privileged effect never happens
		}
		s.RequestShutdown()
		return map[string]any{"stopping": true}, nil
	})

	s.rpc.Handle("auth.passphrase_reset", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
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
		if err := exactArgs(req.Params, 4); err != nil {
			return nil, err
		}
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
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
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
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
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
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
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
	s.rpc.Handle("auth.allowlist_list", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		cfg, rows, err := s.auth.ListAllowedIPs(ctx, token)
		if err != nil {
			return nil, wireErr(err)
		}
		outRows := make([]any, 0, len(rows))
		for _, r := range rows {
			outRows = append(outRows, map[string]any{
				"id": r.ID, "cidr": r.CIDR, "note": r.Note,
				"created_by": r.CreatedBy, "created_at": r.CreatedAt,
			})
		}
		cfgOut := make([]any, 0, len(cfg))
		for _, c := range cfg {
			cfgOut = append(cfgOut, c)
		}
		return map[string]any{"config": cfgOut, "rows": outRows}, nil
	})
	s.rpc.Handle("auth.user_ip_list", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		rows, err := s.auth.UserIPs(ctx, token, userID)
		if err != nil {
			return nil, wireErr(err)
		}
		out := make([]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, map[string]any{
				"id": r.ID, "user_id": r.UserID, "cidr": r.CIDR,
				"label": r.Label, "created_at": r.CreatedAt,
			})
		}
		return out, nil
	})
	s.rpc.Handle("auth.user_ip_add", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 4); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		cidr, err := argStr(req.Params, 2, "cidr")
		if err != nil {
			return nil, err
		}
		label, err := argStr(req.Params, 3, "label")
		if err != nil {
			return nil, err
		}
		// An empty cidr means "the address this request came from" — the
		// self-service enrollment gesture; only the server knows that
		// address truthfully, so the substitution happens here.
		if cidr == "" {
			cidr = peerIP(req)
		}
		return nil, wireErr(s.auth.AddUserIP(ctx, token, userID, cidr, label, peerIP(req)))
	})
	s.rpc.Handle("auth.user_ip_remove", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		userID, err := argInt(req.Params, 1, "user_id")
		if err != nil {
			return nil, err
		}
		rowID, err := argInt(req.Params, 2, "row_id")
		if err != nil {
			return nil, err
		}
		return nil, wireErr(s.auth.RemoveUserIP(ctx, token, userID, rowID, peerIP(req)))
	})

	// --- conn: connection management ---
	s.rpc.Handle("conn.create", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 4); err != nil {
			return nil, err
		}
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
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
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
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
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
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
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
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
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
	}
	// Fixed-size byte ARRAYS never match the []byte case above: a
	// postgres uuid scans into [16]uint8, and the %v fallback would ship
	// it as a decimal byte list. Carry the SAME BYTES as a []byte — how
	// they read (uuid, text, hex) is the frontend's decision.
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Array &&
		rv.Type().Elem().Kind() == reflect.Uint8 {
		b := make([]byte, rv.Len())
		reflect.Copy(reflect.ValueOf(b), rv)
		return b
	}
	// Driver types that know their own text form (pgtype values, decimals,
	// net.IP, …) render through it rather than through %v's struct dump.
	switch x := v.(type) {
	case encoding.TextMarshaler:
		if t, err := x.MarshalText(); err == nil {
			return string(t)
		}
	case fmt.Stringer:
		return x.String()
	}
	return fmt.Sprintf("%v", v)
}

// registerM6 wires the schema.* and workspace.* surface (ADR-0057 §6,
// protocol 2) under the same per-verb discipline as the v1 methods: exact
// arity and typed decoding, peer-IP threading into every mutating core
// call, sentinel-constant-only disclosure, wire-vocabulary normalization.
func (s *Server) registerM6() {
	// --- schema introspection (authz ≥ reader happens in the core, R13) ---
	s.rpc.Handle("schema.schemas", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 1, "conn_id")
		if err != nil {
			return nil, err
		}
		names, err := s.eng.ListSchemas(ctx, token, connID)
		if err != nil {
			return nil, wireErr(err)
		}
		out := make([]any, 0, len(names))
		for _, n := range names {
			out = append(out, n)
		}
		return out, nil
	})
	s.rpc.Handle("schema.tables", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 1, "conn_id")
		if err != nil {
			return nil, err
		}
		schema, err := argStr(req.Params, 2, "schema")
		if err != nil {
			return nil, err
		}
		tables, err := s.eng.ListTables(ctx, token, connID, schema)
		if err != nil {
			return nil, wireErr(err)
		}
		out := make([]any, 0, len(tables))
		for _, t := range tables {
			out = append(out, map[string]any{
				"schema": t.Schema, "name": t.Name,
				"kind": string(t.Kind), "quoted": t.Quoted,
				"partitioned": t.Partitioned, "is_partition": t.IsPartition,
				"parent": t.Parent,
			})
		}
		return out, nil
	})
	s.rpc.Handle("schema.columns", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 4); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 1, "conn_id")
		if err != nil {
			return nil, err
		}
		schema, err := argStr(req.Params, 2, "schema")
		if err != nil {
			return nil, err
		}
		table, err := argStr(req.Params, 3, "table")
		if err != nil {
			return nil, err
		}
		cols, err := s.eng.ListColumns(ctx, token, connID, schema, table)
		if err != nil {
			return nil, wireErr(err)
		}
		out := make([]any, 0, len(cols))
		for _, c := range cols {
			out = append(out, map[string]any{
				"name": c.Name, "type": c.DataType, "nullable": c.Nullable,
				"default": c.Default, "has_default": c.HasDefault,
				"position": int64(c.Position), "pk": c.PrimaryKey,
			})
		}
		return out, nil
	})
	s.rpc.Handle("schema.routines", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 1, "conn_id")
		if err != nil {
			return nil, err
		}
		schema, err := argStr(req.Params, 2, "schema")
		if err != nil {
			return nil, err
		}
		supported, routines, err := s.eng.ListRoutines(ctx, token, connID, schema)
		if err != nil {
			return nil, wireErr(err)
		}
		list := make([]any, 0, len(routines))
		for _, r := range routines {
			list = append(list, map[string]any{
				"schema": r.Schema, "name": r.Name,
				"kind": string(r.Kind), "signature": r.Signature,
			})
		}
		// Capability absence is DATA (supported=false), never an error.
		return map[string]any{"supported": supported, "routines": list}, nil
	})

	s.rpc.Handle("auth.user_list", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		users, err := s.auth.ListUsers(ctx, token)
		if err != nil {
			return nil, wireErr(err)
		}
		out := make([]any, 0, len(users))
		for _, u := range users {
			out = append(out, map[string]any{
				"id": u.ID, "name": u.Name, "role": u.Role, "disabled": u.Disabled,
			})
		}
		return out, nil
	})

	// --- workspaces ---
	s.rpc.Handle("workspace.create", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		name, err := argStr(req.Params, 1, "name")
		if err != nil {
			return nil, err
		}
		id, err := s.eng.CreateWorkspace(ctx, token, name, peerIP(req))
		if err != nil {
			return nil, wireErr(err)
		}
		return id, nil
	})
	s.rpc.Handle("workspace.list", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		views, err := s.eng.ListWorkspaces(ctx, token)
		if err != nil {
			return nil, wireErr(err)
		}
		out := make([]any, 0, len(views))
		for _, w := range views {
			conns := make([]any, 0, len(w.Connections))
			for _, c := range w.Connections {
				conns = append(conns, map[string]any{
					"id": c.ID, "name": c.Name, "engine": c.Engine,
				})
			}
			out = append(out, map[string]any{
				"id": w.ID, "name": w.Name, "created_at": w.CreatedAt,
				"connections": conns,
			})
		}
		return out, nil
	})
	s.rpc.Handle("workspace.rename", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		wsID, err := argInt(req.Params, 1, "workspace_id")
		if err != nil {
			return nil, err
		}
		name, err := argStr(req.Params, 2, "name")
		if err != nil {
			return nil, err
		}
		return nil, wireErr(s.eng.RenameWorkspace(ctx, token, wsID, name, peerIP(req)))
	})
	s.rpc.Handle("workspace.delete", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		wsID, err := argInt(req.Params, 1, "workspace_id")
		if err != nil {
			return nil, err
		}
		return nil, wireErr(s.eng.DeleteWorkspace(ctx, token, wsID, peerIP(req)))
	})
	s.rpc.Handle("workspace.attach", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		wsID, err := argInt(req.Params, 1, "workspace_id")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 2, "conn_id")
		if err != nil {
			return nil, err
		}
		return nil, wireErr(s.eng.AttachConnection(ctx, token, wsID, connID, peerIP(req)))
	})
	s.rpc.Handle("workspace.detach", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		wsID, err := argInt(req.Params, 1, "workspace_id")
		if err != nil {
			return nil, err
		}
		connID, err := argInt(req.Params, 2, "conn_id")
		if err != nil {
			return nil, err
		}
		return nil, wireErr(s.eng.DetachConnection(ctx, token, wsID, connID, peerIP(req)))
	})
}
