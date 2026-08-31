package rpc

import (
	"context"
	"encoding"
	"errors"
	"fmt"
	"reflect"
	"strings"
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

	// The ExecSession family (protocol 5, ADR-0074 §8a). Protocol 5 added
	// the session verbs but no codes for what they can refuse, so every
	// session error — no such session, already running, cap reached —
	// fell through wireErr as an unmapped error and reached the client as a
	// generic internal failure. A client cannot act on that: "the server
	// broke" and "you already have eight sessions open" call for opposite
	// responses, and only one of them is worth retrying.
	//
	// The split is by what the CLIENT should do, which is the only thing a
	// code is for:

	// CodeSessionNotFound: the session does not exist, or is not this
	// caller's. Deliberately one code for both — the id space must not
	// become a way to discover which sessions exist. Reopen.
	CodeSessionNotFound int64 = -32040
	// CodeSessionBusy: one in-flight statement per session, and this
	// session already has one. Wait for the previous call, or use another
	// session; the request was not run.
	CodeSessionBusy int64 = -32041
	// CodeSessionCapExceeded: the per-user or global session cap is full.
	// Close a session, or wait for one to be reaped. Retrying immediately
	// will fail the same way.
	CodeSessionCapExceeded int64 = -32042
	// CodeTxState: the request is wrong for the transaction's CURRENT state
	// — no transaction open, one already open, or an aborted transaction
	// that accepts only ROLLBACK. The fix is a different statement, not a
	// retry of this one.
	CodeTxState int64 = -32043
	// CodeConnectionDraining: the connection is being deleted or shut down.
	// Nothing on it will succeed again; this is not a retry.
	CodeConnectionDraining int64 = -32044
	// CodeNoSuchTx: tx.status was asked about a transaction id with no
	// record — or one belonging to someone else, which is deliberately
	// indistinguishable. Not a pending status: a mistyped or expired id must
	// not leave a caller polling forever for a transaction that never was.
	CodeNoSuchTx int64 = -32045
	// CodeInvalidToken carries PAT MANAGEMENT refusals — a duplicate name, a
	// cap reached, a lifetime out of range, an allowed_ips that is not a
	// subset. The caller is authenticated and managing their own tokens, so
	// the specific reason is theirs to see.
	//
	// It is emphatically NOT the front door's credential failure. That one
	// is uniform and anonymous by design, and it never travels this wire.
	CodeInvalidToken int64 = -32046
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

	// The session/transaction surface (protocol 5). Each of these is a
	// condition the CALLER can do something about, which is why they are
	// public: their constant text says what happened and the code says what
	// to do about it. None of them names a connection, a user, or another
	// session, so publishing them discloses nothing about what exists.
	{exec.ErrSessionNotFound, CodeSessionNotFound},
	// A transaction that is not the caller's answers exactly as one that
	// never existed — same sentinel, same code, same text — so tx.status
	// cannot be used to discover which transaction ids exist.
	{exec.ErrNoSuchTx, CodeNoSuchTx},

	// PAT management failures. These reach an AUTHENTICATED caller managing
	// their own tokens, so naming them is safe and useful — unlike
	// auth.ErrPATInvalid, which is the anonymous front-door path's single
	// uniform failure and is deliberately absent from this list.
	{auth.ErrPATNameTaken, CodeInvalidToken},
	{auth.ErrPATCapExceeded, CodeInvalidToken},
	{auth.ErrPATBadExpiry, CodeInvalidToken},
	{auth.ErrPATBadAllowedIPs, CodeInvalidToken},
	{exec.ErrSessionBusy, CodeSessionBusy},
	{exec.ErrSessionCapExceeded, CodeSessionCapExceeded},
	{exec.ErrConnectionDraining, CodeConnectionDraining},

	// Transaction-state refusals all map to one code: the caller's next
	// move is the same in every case — send a different statement, not this
	// one again — and the sentinel's own text says which state it was in.
	{exec.ErrTxAlreadyOpen, CodeTxState},
	{exec.ErrNoOpenTx, CodeTxState},
	{exec.ErrTxAborted, CodeTxState},
	{exec.ErrTxChainUnsupported, CodeTxState},

	// The session-state gate (SET / LOCK). These are refusals of a specific
	// statement, like the classification gate above, so they carry its code
	// rather than a new one — a caller treats them the same way.
	{exec.ErrSetNotLocal, CodeStatementRejected},
	{exec.ErrSetGUCRefused, CodeStatementRejected},
	{exec.ErrSetOutsideTx, CodeStatementRejected},
	{exec.ErrLockOutsideTx, CodeStatementRejected},
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
		// The ATOMIC path (R5 gate). A script containing a transaction
		// boundary runs inside one transaction on an ephemeral session; a
		// script without one behaves exactly as it always has, statement by
		// statement. This is the one call site — the editor surfaces (Lua,
		// TUI, Web) all reach the engine through here, so making them
		// atomic is not three changes.
		out, xerr := s.eng.ExecuteScriptAtomic(ctx, token, connID, sqlText, peerIP(req))
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
	s.rpc.Handle("auth.user_allowlist_list", func(ctx context.Context, req *golibrpc.Request) (any, error) {
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
	// Personal Access Tokens (ADR-0075 §4). The credential a person pastes
	// into a DSN, managed from the surfaces they already use.
	//
	// The secret appears exactly once, in this reply. It is not recoverable
	// afterwards from anywhere — the store keeps a selector and a SHA-256 —
	// so the reply is the only chance to copy it, and the client is expected
	// to say so.
	s.rpc.Handle("auth.token_create", func(ctx context.Context, req *golibrpc.Request) (any, error) {
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
		days, err := argInt(req.Params, 2, "days")
		if err != nil {
			return nil, err
		}
		rawIPs, err := argStr(req.Params, 3, "allowed_ips")
		if err != nil {
			return nil, err
		}
		var ips []string
		if strings.TrimSpace(rawIPs) != "" {
			ips = strings.Split(rawIPs, ",")
		}
		out, cerr := s.auth.CreatePAT(ctx, token, name, time.Duration(days)*24*time.Hour, ips)
		if cerr != nil {
			return nil, wireErr(cerr)
		}
		return map[string]any{
			"name": out.Name,
			// The one and only time this value exists outside the client's
			// hands.
			"secret":     out.Secret,
			"expires_at": out.ExpiresAt.Format(time.RFC3339),
		}, nil
	})

	s.rpc.Handle("auth.token_list", func(ctx context.Context, req *golibrpc.Request) (any, error) {
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
		rows, lerr := s.auth.ListPATs(ctx, token, userID)
		if lerr != nil {
			return nil, wireErr(lerr)
		}
		out := make([]any, 0, len(rows))
		for _, r := range rows {
			// Never the digest and never the selector. The digest is
			// useless to a client and the selector is half the credential:
			// publishing it would turn a token list into a head start.
			out = append(out, map[string]any{
				"name":        r.Name,
				"created_at":  time.Unix(r.CreatedAt, 0).Format(time.RFC3339),
				"expires_at":  time.Unix(r.ExpiresAt, 0).Format(time.RFC3339),
				"last_used":   lastUsedString(r.LastUsedAt),
				"revoked":     r.IsRevoked(),
				"allowed_ips": r.AllowedIPs,
			})
		}
		return out, nil
	})

	s.rpc.Handle("auth.token_revoke", func(ctx context.Context, req *golibrpc.Request) (any, error) {
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
		name, err := argStr(req.Params, 2, "name")
		if err != nil {
			return nil, err
		}
		if rerr := s.auth.RevokePAT(ctx, token, userID, name); rerr != nil {
			return nil, wireErr(rerr)
		}
		return map[string]any{"revoked": true}, nil
	})

	s.rpc.Handle("auth.user_allowlist_add", func(ctx context.Context, req *golibrpc.Request) (any, error) {
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
	s.rpc.Handle("auth.user_allowlist_remove", func(ctx context.Context, req *golibrpc.Request) (any, error) {
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
	// The ExecSession surface (ADR-0074 §3, R5). A session is the client's
	// handle on a PINNED connection: it is what makes a transaction spanning
	// several round trips possible at all, since the stateless path may put
	// each statement on a different physical connection.
	//
	// The session id is opaque and engine-issued. It is deliberately NOT the
	// auth token and NOT the TCP connection: a client may hold several, and
	// one dying must not take the others with it.
	s.rpc.Handle("exec.session_open", func(ctx context.Context, req *golibrpc.Request) (any, error) {
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
		sid, oerr := s.eng.OpenSession(ctx, token, connID, peerIP(req))
		if oerr != nil {
			return nil, wireErr(oerr)
		}
		return map[string]any{"session_id": string(sid)}, nil
	})

	// Closing is not optional housekeeping: an open session holds a physical
	// connection, and closing rolls back any transaction still open on it.
	// A client that crashes without calling this is reaped by the engine's
	// idle timeout — this verb is the polite path, not the only one.
	s.rpc.Handle("exec.session_close", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		sid, err := argStr(req.Params, 1, "session_id")
		if err != nil {
			return nil, err
		}
		if cerr := s.eng.CloseSession(ctx, token, exec.SessionID(sid), peerIP(req)); cerr != nil {
			return nil, wireErr(cerr)
		}
		return map[string]any{"closed": true}, nil
	})

	// Session-scoped run. The connection is the SESSION's — the caller does
	// not pass one, and cannot redirect a session at another connection
	// mid-transaction.
	s.rpc.Handle("exec.session_run", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		sid, err := argStr(req.Params, 1, "session_id")
		if err != nil {
			return nil, err
		}
		sqlText, err := argStr(req.Params, 2, "sql")
		if err != nil {
			return nil, err
		}
		res, rerr := s.eng.SessionExecute(ctx, token, exec.SessionID(sid), sqlText, peerIP(req))
		if rerr != nil {
			return nil, wireErr(rerr)
		}
		return resultMap(res), nil
	})

	// tx.status is the poll verb §7 names for asynchronous outcome delivery
	// (ADR-0074 Amendment 4 A2, R5 scope).
	//
	// It reads the ENGINE's outcome API, never the table, and never the
	// script history — history disappears entirely when [history].enabled is
	// false, and a boundary-only `BEGIN; COMMIT;` never had a row there at
	// all, so a projection over it could not answer for the two cases that
	// most need answering.
	//
	// One tx id gives that transaction's resolved state; no id gives the
	// caller's unresolved ones, oldest first. Both are scoped in core: a
	// transaction that is not the caller's answers exactly as one that never
	// existed, so the id space cannot be used to discover what exists.
	s.rpc.Handle("tx.status", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		n := len(req.Params)
		if n != 2 && n != 3 {
			return nil, &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
				Message: "tx.status takes (token, tx_id) or (token, \"\", limit)"}
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		txID, err := argStr(req.Params, 1, "tx_id")
		if err != nil {
			return nil, err
		}
		if txID != "" {
			// The single-transaction form takes exactly two arguments. A
			// third alongside a non-empty id is not a defined shape, and
			// accepting it silently would let a caller write
			// tx.status(token, id, limit) — a reading that looks obvious and
			// means nothing here — and receive an answer about the id while
			// their limit was quietly discarded. A verb that ignores an
			// argument teaches the wrong contract to whoever copies the call.
			if n != 2 {
				return nil, &golibrpc.Error{Code: golibrpc.CodeInvalidParams,
					Message: "tx.status with a tx_id takes exactly (token, tx_id); " +
						"a limit applies only to the pending form (token, \"\", limit)"}
			}
			st, terr := s.eng.TxOutcome(ctx, token, txID)
			if terr != nil {
				return nil, wireErr(terr)
			}
			return txStatusMap(st), nil
		}
		limit := 0
		if n == 3 {
			l, lerr := argInt(req.Params, 2, "limit")
			if lerr != nil {
				return nil, lerr
			}
			limit = int(l)
		}
		list, perr := s.eng.PendingOutcomes(ctx, token, limit)
		if perr != nil {
			return nil, wireErr(perr)
		}
		out := make([]any, 0, len(list))
		for _, st := range list {
			out = append(out, txStatusMap(st))
		}
		return map[string]any{"pending": out}, nil
	})

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

// txStatusMap projects one transaction outcome onto the wire.
//
// `terminal` is computed HERE and not carried on the Go struct: on the engine
// side it is a method, because a precomputed field can be constructed
// inconsistently with the state it is derived from. A Lua or Web client
// cannot call a Go method, so this is the layer where precomputing earns its
// keep — one place, derived from the state it ships alongside.
//
// Times go out as RFC3339Nano strings like every other time on this wire, and
// `stuck_ms` is the gap between them: how long this transaction has been in
// its current state is the number that decides whether to act, and making
// each client subtract two timestamps is how three clients get three answers.
func txStatusMap(st exec.TxStatus) map[string]any {
	return map[string]any{
		"tx_id":    st.TxID,
		"state":    string(st.State),
		"reason":   st.Reason,
		"conn_id":  st.ConnID,
		"user_id":  st.UserID,
		"terminal": st.Terminal(),
		"opened":   st.Opened.Format(time.RFC3339Nano),
		"since":    st.Since.Format(time.RFC3339Nano),
		"stuck_ms": time.Since(st.Since).Milliseconds(),
	}
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

// lastUsedString renders a PAT's last-use time, or "never".
//
// Zero is not a date. Rendering it as 1970 would put a plausible-looking
// timestamp on a token nobody has used, which is exactly the row an operator
// is scanning for when they are deciding what to revoke.
func lastUsedString(unix int64) string {
	if unix == 0 {
		return "never"
	}
	return time.Unix(unix, 0).Format(time.RFC3339)
}
