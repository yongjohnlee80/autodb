package frontdoor

import (
	"fmt"
	"strings"
)

// ATTEMPT-BEFORE-EFFECT ON THE FRONT-DOOR EVENT FEED (matrix §1.3, row
// 4:Execute; Johno's ruling 2026-09-05: emit it).
//
// TWO SINKS, and this is the one that was missing. The DURABLE record has
// always existed — core/exec's recordAttemptTagged writes an `exec` audit row
// plus a pending history row before dispatch, on every path (wire_query.go,
// wire_execute.go, and wire_extended.go inside WireExecutePortal). What the
// listener's OnEvent feed carried was fd.stmt_outcome with no counterpart, so
// an operator watching the stream saw outcomes it could not pair with anything
// and the matrix named an event the stream did not carry.
//
// The feed is OPERATIONAL, not the audit trail: it is best-effort, it is not
// transactional with the effect, and nothing may be inferred from its absence
// after a crash — that is what the audit_log and history rows are for. So this
// emits BEFORE dispatch and does not fail the statement if the consumer is
// slow; the durable guarantee is unchanged and lives where it always did.
//
// DETAIL SHAPE. `<phase> <identifier>; sql=<clipped>` for the phases that carry
// SQL, `<phase> <identifier>` for the ones that do not. The phase is what makes
// three call sites distinguishable in one stream: a `query` attempt is one
// statement of a simple Query, a `parse` attempt is the parse-time gate, and an
// `execute` attempt is one Execute of one portal — including a re-execution of
// a suspended portal, which the matrix contracts as its own attempt and which
// is the half row 4:Execute was open on.

// attemptSQLBytes bounds what reaches the event feed. Consumers of this stream
// are log shippers and TUI tails, not forensic tooling; the untruncated
// statement is in the history row.
const attemptSQLBytes = 512

// stmtAttempt emits one fd.stmt_attempt. Called BEFORE the frame that could
// have an effect is dispatched — never after, and never conditionally on the
// outcome, or the event stops meaning "this was tried".
func (l *Listener) stmtAttempt(peer, phase, ident, sql string) {
	detail := phase
	if ident != "" {
		detail += " " + ident
	}
	if sql != "" {
		detail += "; sql=" + clipSQL(sql, attemptSQLBytes)
	}
	l.onEvent(Event{Kind: "fd.stmt_attempt", Peer: peer, Detail: detail})
}

// clipSQL bounds a statement for the event feed and SAYS when it clipped, so a
// reader can tell a short statement from a truncated one. It clips on a rune
// boundary: a detail cut mid-rune renders as a replacement character and a
// reader cannot tell whether that came from the statement or from us.
func clipSQL(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s…(+%d bytes)", s[:cut], len(s)-cut)
}

// utf8RuneStart reports whether b begins a rune (i.e. is not a continuation
// byte 10xxxxxx).
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// portalIdent names an Execute's subject the way the protocol does: the unnamed
// portal is the empty string on the wire and "" is unreadable in a log line, so
// it is rendered as the protocol's own term for it.
func portalIdent(kind, name string) string {
	if name == "" {
		return kind + "=<unnamed>"
	}
	return kind + "=" + name
}
