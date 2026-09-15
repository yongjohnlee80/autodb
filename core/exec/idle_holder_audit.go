package exec

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The thirty-minute idle-in-transaction heartbeat.
//
// A transaction may now sit idle for two hours and run for eight. Those bounds
// were raised deliberately -- a developer debugging needs to think, not to race
// a ninety-second clock -- but a bound that generous is only safe if somebody
// can SEE what is holding the lock while it holds it. Before this, an idle
// holder was invisible until the moment it was reclaimed, so the audit trail
// recorded the ending and never the eight hours of waiting that led to it.
//
// So the holder announces itself every thirty minutes, with enough identity to
// act on: who, from where, since when, holding what, and what they last ran.
// It is an audit occurrence and NOT a page; whether anybody is woken is an
// operations decision made elsewhere.
const (
	// idleHolderAction is a SEPARATE audit identity from the reclamation that
	// may eventually follow. An operator asking "what was held, and for how
	// long" and an operator asking "what did we end, and why" are asking
	// different questions, and one identity serving both answers neither.
	idleHolderAction = "idle_in_transaction_holder"

	// idleHolderCadence is the heartbeat interval: 30m, 60m, 90m, and so on
	// until the episode ends (Johno's ruling, 2026-09-15).
	idleHolderCadence = 30 * time.Minute

	// statementPreviewBytes bounds the ESCAPED preview. An audit row is not a
	// place to store a megabyte of generated SQL, and an unbounded field in a
	// record emitted every thirty minutes per holder is a slow way to fill a
	// disk.
	statementPreviewBytes = 256

	// holderNull is what an inapplicable or unavailable field renders as.
	//
	// EXPLICIT, never a fabricated zero. A missing acquisition time rendered
	// as `acquired=0` reads as the Unix epoch, and an absent username rendered
	// as `user=` reads as an empty username -- both are answers, and the true
	// answer is that nobody knows.
	holderNull = "null"
)

// statementPreview renders SQL for an audit field: escaped, valid UTF-8, and
// bounded, with no code point split across the cap.
//
// Naive truncation is `s[:256]`, and on a statement containing any multibyte
// character that lands, sooner or later, in the middle of one -- writing
// invalid UTF-8 into the meta store, where the failure surfaces far from here
// as a row that will not decode. The cap is therefore applied to the ESCAPED
// output and only ever at a boundary between whole escaped runes.
//
// Control characters are escaped rather than passed through, because a preview
// containing a raw newline or a terminal escape sequence is a field that can
// forge the shape of the record that contains it.
func statementPreview(sql string) (preview string, truncated bool) {
	var b strings.Builder
	for _, r := range sql {
		esc := escapeRune(r)
		// The whole escape is atomic: a cap that admitted `\x0` and dropped
		// the final digit would be the byte-splitting defect wearing a
		// different hat.
		if b.Len()+len(esc) > statementPreviewBytes {
			return b.String(), true
		}
		b.WriteString(esc)
	}
	return b.String(), false
}

func escapeRune(r rune) string {
	switch r {
	case '\\':
		return `\\`
	case '"':
		return `\"`
	case '\n':
		return `\n`
	case '\r':
		return `\r`
	case '\t':
		return `\t`
	}
	// RuneError covers two cases that must render identically: a genuine
	// U+FFFD in the source, and an invalid byte that ranging over the string
	// reported as one. Neither is something to pass through verbatim.
	if r < 0x20 || r == 0x7f {
		return fmt.Sprintf(`\x%02x`, r)
	}
	return string(r)
}

// statementFingerprint identifies the statement without disclosing it: a
// stable short digest, so two holders running the same query are visibly
// running the same query even where the preview was truncated.
//
// It is a hash, so it carries no literal and no bind value out of the
// statement, and it is taken over the FULL text rather than the preview so
// truncation cannot make two different statements look identical.
func statementFingerprint(sql string) string {
	if sql == "" {
		return holderNull
	}
	sum := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(sum[:8])
}

// idleHolder is one holder's identity at the moment a heartbeat fires.
//
// PATs, passwords, cancel secrets and bind values are absent BY CONSTRUCTION:
// there is no field here that could carry one. The PAT is identified by its
// row id, which is what an operator needs to revoke it and is useless to
// anyone who steals the audit trail.
type idleHolder struct {
	session SessionID
	connID  int64

	subject  int64  // the authenticated user's row id
	username string // "" when the session never resolved one
	ip       string
	patID    int64 // the PAT's ROW ID; 0 when the session is not PAT-backed

	appName  string // client-supplied, ADVISORY ONLY; never a decision input
	mayWrite bool

	txID    string
	txPhase txPhase

	acquiredAt   time.Time // when this session took its backend
	txOpened     time.Time
	lastActivity time.Time
	deadline     time.Time // the effective bound, whichever fires first

	idle  time.Duration
	txAge time.Duration

	stmts          int
	accountHolders int

	lastSQL string

	interval time.Duration // which heartbeat this is: 30m, 60m, 90m, ...
}

// render writes the record as ordered key=value pairs.
//
// Ordered and flat rather than nested, because the audit detail is one string
// column and the thing an operator does with it is grep it. Every field is
// present in every record, including the ones that are null, so a search for
// `pid=` finds the holders where it is unknown instead of silently skipping
// them.
func (h idleHolder) render() string {
	preview, truncated := statementPreview(h.lastSQL)

	fields := []struct{ k, v string }{
		{"heartbeat", durationField(h.interval)},
		{"session", string(h.session)},
		{"conn", strconv.FormatInt(h.connID, 10)},
		{"subject", strconv.FormatInt(h.subject, 10)},
		{"user", quoteOrNull(h.username)},
		{"ip", quoteOrNull(h.ip)},
		{"pat", idOrNull(h.patID)},
		// The BACKEND PID IS NOT AVAILABLE, and says so rather than being
		// omitted. Nothing in the pinned-connection seam exposes it (golib's
		// PinnedConn has no accessor), so an operator reading this record
		// cannot yet map a holder to a row in pg_stat_activity. Recording the
		// gap is what makes it a known gap instead of a field nobody noticed
		// was missing.
		{"pid", holderNull},
		{"target", strconv.FormatInt(h.connID, 10)},
		{"role", roleName(h.mayWrite)},
		{"app", quoteOrNull(h.appName)},
		{"tx", quoteOrNull(h.txID)},
		{"tx_state", txStateLetter(h.txPhase)},
		{"acquired_at", stampOrNull(h.acquiredAt)},
		{"tx_started_at", stampOrNull(h.txOpened)},
		{"last_activity_at", stampOrNull(h.lastActivity)},
		// R7's dependency-progress clock does not exist yet; the ladder rung
		// that would advance it is gated. Null, not zero.
		{"last_dependency_progress_at", holderNull},
		{"deadline_at", stampOrNull(h.deadline)},
		{"idle_age", durationField(h.idle)},
		{"tx_age", durationField(h.txAge)},
		{"statements", strconv.Itoa(h.stmts)},
		{"account_holders", strconv.Itoa(h.accountHolders)},
		{"last_statement_fingerprint", statementFingerprint(h.lastSQL)},
		{"last_statement", quoteOrNull(preview)},
		{"last_statement_truncated", strconv.FormatBool(truncated)},
	}

	var b strings.Builder
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(f.k)
		b.WriteByte('=')
		b.WriteString(f.v)
	}
	return b.String()
}

// durationField renders an age for a human reader.
//
// Truncated to the second, because these are measured against a clock that was
// stamped by a different code path and the sub-second remainder is the gap
// between the two, not information about the holder. `idle_age=30m0.00000019s`
// is also simply worse to read and to grep for than `idle_age=30m0s`.
func durationField(d time.Duration) string {
	return d.Truncate(time.Second).String()
}

func quoteOrNull(s string) string {
	if s == "" {
		return holderNull
	}
	// Already escaped for the preview; every other string here is an
	// identifier, a username or an address. Quoting keeps a value containing
	// a space from splitting the record into two fields.
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

func idOrNull(id int64) string {
	if id == 0 {
		return holderNull
	}
	return strconv.FormatInt(id, 10)
}

func stampOrNull(t time.Time) string {
	if t.IsZero() {
		return holderNull
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func roleName(mayWrite bool) string {
	if mayWrite {
		return "writer"
	}
	return "reader"
}

// txStateLetter uses PostgreSQL's own ReadyForQuery vocabulary, so the field
// means the same thing here as it does on the wire the client is watching.
func txStateLetter(p txPhase) string {
	switch p {
	case txActive:
		return "T"
	case txAborted:
		return "E"
	default:
		return holderNull
	}
}
