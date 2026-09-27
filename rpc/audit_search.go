package rpc

import (
	"context"
	"time"

	golibrpc "github.com/yongjohnlee80/golib/server/rpc"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// audit.search: the audit log, filtered and paged, for an admin. Added with
// protocol 9. Answers {rows, next, conn_filter_since}: rows written before the
// audit log recorded connections name none, so a connection or workspace
// filter covers rows from conn_filter_since on, and a client says so.

// auditFilterFrom decodes audit.search's filter map, as strictly as
// history.search's: a key it does not know is refused, never ignored.
func auditFilterFrom(raw any) (exec.AuditFilter, error) {
	var f exec.AuditFilter
	if raw == nil {
		return f, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return f, invalid("filter: want a map, got %T", raw)
	}
	num := func(k string) (int64, error) {
		n, ok := m[k].(int64)
		if !ok {
			return 0, invalid("filter.%s: want an integer, got %T", k, m[k])
		}
		return n, nil
	}
	for k, v := range m {
		var err error
		switch k {
		case "connection_id":
			f.ConnID, err = num(k)
		case "workspace_id":
			f.WorkspaceID, err = num(k)
		case "user_id":
			f.UserID, err = num(k)
		case "limit":
			var n int64
			n, err = num(k)
			f.Limit = int(n)
		case "from", "to": // unix seconds
			var n int64
			if n, err = num(k); err == nil {
				if k == "from" {
					f.From = time.Unix(n, 0)
				} else {
					f.To = time.Unix(n, 0)
				}
			}
		case "actions":
			list, ok := v.([]any)
			if !ok {
				return f, invalid("filter.actions: want a list, got %T", v)
			}
			for _, a := range list {
				s, ok := a.(string)
				if !ok {
					return f, invalid("filter.actions: want strings, got %T", a)
				}
				f.Actions = append(f.Actions, s)
			}
		case "before":
			c, ok := v.(map[string]any)
			if !ok {
				return f, invalid("filter.before: want a map, got %T", v)
			}
			at, ok1 := c["created_at"].(int64)
			id, ok2 := c["id"].(int64)
			if !ok1 || !ok2 || len(c) != 2 {
				return f, invalid("filter.before: want {created_at, id} integers")
			}
			f.Before = &exec.AuditCursor{CreatedAt: at, ID: id}
		default:
			return f, invalid("filter: unknown key %q", k)
		}
		if err != nil {
			return f, err
		}
	}
	return f, nil
}

func (s *Server) registerAuditSearch() {
	s.handle("audit.search", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		f, err := auditFilterFrom(req.Params[1])
		if err != nil {
			return nil, err
		}
		page, aerr := s.eng.SearchAudit(ctx, token, f)
		if aerr != nil {
			return nil, s.wireErr(aerr)
		}
		rows := make([]any, 0, len(page.Rows))
		for _, r := range page.Rows {
			rows = append(rows, map[string]any{
				"id": r.ID, "user_id": r.UserID, "user": r.User, "ip": r.IP,
				"action": r.Action, "detail": r.Detail,
				"connection_id": r.ConnID, "connection": r.Conn,
				"tx_id": r.TxID, "attempt_id": r.AttemptID,
				"created_at": r.CreatedAt.Format(time.RFC3339),
			})
		}
		var next, since any
		if page.Next != nil {
			next = map[string]any{"created_at": page.Next.CreatedAt, "id": page.Next.ID}
		}
		if !page.ConnFilterSince.IsZero() {
			since = page.ConnFilterSince.Unix()
		}
		return map[string]any{"rows": rows, "next": next, "conn_filter_since": since}, nil
	})
}
