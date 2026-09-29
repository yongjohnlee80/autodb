package rpc

import (
	"context"
	"fmt"
	"time"

	golibrpc "github.com/yongjohnlee80/golib/server/rpc"

	"github.com/yongjohnlee80/autodb/core/exec"
	"github.com/yongjohnlee80/autodb/core/meta"
)

// history.search and dispositions.list: the history read side with filters
// and a cursor, and the disposition counts. Added with protocol 9.
//
// history.list keeps its shape (a limit in, a list out) for the clients that
// use it; history.search is the paged, filtered form, answering
// {rows, next} with next nil at the end.

// historyRowWire is one history row as the wire carries it.
func historyRowWire(r exec.HistoryRow) map[string]any {
	return map[string]any{
		"id": r.ID, "user_id": r.UserID, "user": r.User,
		"connection_id": r.ConnID, "connection": r.Conn, "ip": r.IP,
		"script": r.Script, "started_at": r.StartedAt.Format(time.RFC3339),
		"duration_ms": r.Duration.Milliseconds(), "row_count": r.RowCount,
		"status": r.Status, "error": r.Error,
		// A SEPARATE KEY, never a fifth status value: status is the
		// durability token and a suspended Execute did commit. A client that
		// does not know the key reads the same status it always did.
		"suspended": r.Suspended,
		// What the attempt did, beside what became of its effect; "" for a
		// row that finished before dispositions.
		"disposition": r.Disposition,
	}
}

// historyFilterFrom decodes history.search's filter map.
//
// STRICT: a key it does not know is refused, not ignored. A filter the server
// silently dropped answers MORE rows than the caller asked for, and a caller
// reading a narrowed listing has no way to see that it is not one.
func historyFilterFrom(raw any) (exec.HistoryFilter, error) {
	var f exec.HistoryFilter
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
		case "status":
			list, ok := v.([]any)
			if !ok {
				return f, invalid("filter.status: want a list, got %T", v)
			}
			for _, s := range list {
				str, ok := s.(string)
				if !ok {
					return f, invalid("filter.status: want strings, got %T", s)
				}
				f.Status = append(f.Status, meta.HistoryStatus(str))
			}
		case "before":
			c, ok := v.(map[string]any)
			if !ok {
				return f, invalid("filter.before: want a map, got %T", v)
			}
			at, ok1 := c["started_at"].(int64)
			id, ok2 := c["id"].(int64)
			if !ok1 || !ok2 || len(c) != 2 {
				return f, invalid("filter.before: want {started_at, id} integers")
			}
			f.Before = &exec.HistoryCursor{StartedAt: at, ID: id}
		default:
			return f, invalid("filter: unknown key %q", k)
		}
		if err != nil {
			return f, err
		}
	}
	return f, nil
}

func invalid(format string, a ...any) error {
	return &golibrpc.Error{Code: golibrpc.CodeInvalidParams, Message: fmt.Sprintf(format, a...)}
}

func (s *Server) registerHistorySearch() {
	s.handle("history.search", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		f, err := historyFilterFrom(req.Params[1])
		if err != nil {
			return nil, err
		}
		rows, next, herr := s.eng.SearchHistory(ctx, token, f)
		if herr != nil {
			return nil, s.wireErrFor(req, herr)
		}
		out := make([]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, historyRowWire(r))
		}
		var nextWire any
		if next != nil {
			nextWire = map[string]any{"started_at": next.StartedAt, "id": next.ID}
		}
		return map[string]any{"rows": out, "next": nextWire}, nil
	})

	s.handle("dispositions.list", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		c, cerr := s.eng.DispositionCounts(ctx, token)
		if cerr != nil {
			return nil, s.wireErrFor(req, cerr)
		}
		if c.HistoryDisabled {
			// Not zeros: with history off nothing is promised to count.
			return map[string]any{"history": "disabled"}, nil
		}
		counts := map[string]any{}
		for d, n := range c.Counts {
			counts[string(d)] = n
		}
		var since any
		if !c.Since.IsZero() {
			since = c.Since.Unix()
		}
		return map[string]any{
			"history": "enabled", "since": since, "counts": counts,
			"in_flight": c.InFlight, "before_dispositions": c.BeforeDispositions,
			"unknown_since_start":     c.UnknownSinceStart,
			"unknown_ids_since_start": strsToAny(c.UnknownIDsSinceStart),
		}, nil
	})
}
