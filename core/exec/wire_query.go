package exec

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// THE F1 WIRE SEAM (ADR-0075 F1; ADR-0018 Amendment 1).
//
// The front-door loop (frontdoor/) never sees a database connection, a raw
// pinned handle, or a pgproto3 message for TARGET data. It sees this: a
// stream of neutral messages produced by the engine AFTER the engine's own
// gate — classifier, capability profile, grants, and the F3a unit policy —
// has accepted the SQL text. Lector's ruling (2026-09-02): gate and dispatch
// are ONE core/exec-owned operation over the exact same bytes, and frontdoor
// must not receive the raw pinned capability. This file is where that
// boundary lives.
//
// The vocabulary mirrors golib's ExtendedMessage kinds so that, when the raw
// path lands (ADR-0018 Amendment 1, SimpleQuerier), the conversion is a field
// copy and the loop does not change.

// WireMessage is one backend message of a Query's response, as protocol data.
//
// Kinds: "RowDescription" (Fields), "DataRow" (Values), "CommandComplete"
// (Tag), "EmptyQueryResponse", "ErrorResponse" (Err — the TARGET's error,
// verbatim, as protocol data and never a Go error), "NoticeResponse" (Notice),
// "ParameterStatus" (ParameterName/ParameterValue), "NotificationResponse"
// (Notification).
//
// NEVER "ReadyForQuery". Readiness is the SESSION's fact, not the target's,
// and a producer that could emit its own readiness could tell the client
// "idle" while the engine holds an open transaction. WireQuery returns the
// status byte separately, from the session state machine (WireTxStatus).
type WireMessage struct {
	Kind string

	// Fields is the RowDescription's column descriptors, in projection order.
	Fields []WireField
	// Values is the DataRow's column payloads. A NULL is a nil slice; an empty
	// non-NULL value is a zero-length non-nil slice (the RawRows rule). On the
	// raw path these are BORROWED for the duration of the emit call; a kept row
	// is copied with bytes.Clone.
	Values [][]byte
	// Tag is the CommandComplete's command tag, verbatim.
	Tag string

	Err          *pgconn.PgError
	Notice       *pgconn.Notice
	Notification *pgconn.Notification

	ParameterName, ParameterValue string
}

// WireField is a RowDescription column descriptor — the same shape golib's
// ExtendedFieldDescription carries, re-declared here because frontdoor must not
// import the raw capability's package.
type WireField struct {
	Name         string
	TableOID     uint32
	ColumnAttr   uint16
	TypeOID      uint32
	TypeSize     int16
	TypeModifier int32
	// Format is the wire format of the values: 0 text, 1 binary.
	Format int16
}

var (
	// ErrWireEmitNil is returned BEFORE any dispatch when WireQuery is handed a
	// nil emit: no frame is sent, no gate is consulted, nothing is audited.
	// (ADR-0018 Amendment 1, A1-C3.)
	ErrWireEmitNil = errors.New("exec: WireQuery requires a non-nil emit")

	// ErrInterimTruncated is INTERIM ONLY. The current producer decodes rows
	// through the paged Result, and a result past the page cannot be served
	// verbatim. Rather than silently drop rows — the exact thing matrix §5
	// forbids — the interim body REFUSES the statement with this error, which
	// the loop frames as a gate error (§8a) with the rule id below. It is
	// removed with the raw path, which has no page.
	ErrInterimTruncated = errors.New("exec: result exceeds the interim producer's page; the raw path (ADR-0018 Amendment 1) removes this limit")
)

// InterimTruncatedRuleID is the §8a DETAIL rule id the loop attaches to
// ErrInterimTruncated. Interim only.
const InterimTruncatedRuleID = "frontdoor/interim-result-page-exceeded"

// WireQuery gates sqlText and, if admitted, dispatches it, calling emit for
// every backend message of the response in wire order, and returns the
// session's ReadyForQuery status byte.
//
// CONTRACT (stable across the interim and raw producers; ADR-0018 A1-C3/C4):
//   - a nil emit fails before dispatch with ErrWireEmitNil;
//   - emit is called synchronously, in order, while WireQuery HOLDS the
//     session's one-in-flight claim — so a callback that re-enters the
//     engine on this session gets ErrSessionBusy, not a second statement.
//     Non-reentrancy is enforced, not requested (lector PR #48 r0 MF1);
//   - a non-nil error from emit stops delivery and is returned (the raw
//     producer first drains the wire to quiescent; the interim one has
//     nothing on the wire to drain);
//   - a TARGET error arrives as WireMessage{Kind: "ErrorResponse", Err} —
//     protocol data — and WireQuery then returns the status normally;
//   - a GATE or SESSION refusal (denied, classifier, size, session gone) is
//     returned as a Go error with no messages emitted: the loop synthesizes
//     the §8a ErrorResponse for those, because they are the front door's
//     own answer, not the target's;
//   - ReadyForQuery is never emitted; the status byte is the return value.
//
// INTERIM BODY — cited for NOTHING in the protocol matrix. Until ADR-0018
// Amendment 1 ships, this wraps WireExecute's decoded Result and re-encodes it
// as text. It is honest about what it cannot do: results past the page are
// REFUSED (ErrInterimTruncated), not truncated; column types are reported as
// text (OID 25) because the decoded Result carries no type information; and it
// does NOT split multi-statement text — the classifier refuses it
// (ErrMultiStatement), so 4:Query's split-and-run semantics are not provided
// here and must not be cited from here. The raw producer replaces only this
// body; the signature, the vocabulary and the contract above do not change.
func (e *Engine) WireQuery(ctx context.Context, id SessionID, userID int64, sqlText, ip string, emit func(WireMessage) error) (byte, error) {
	if emit == nil {
		return 0, ErrWireEmitNil
	}
	s, err := e.sessions.lookup(id, userID)
	if err != nil {
		return 0, err
	}
	// ONE claim for the whole operation: gate, dispatch, every emit, and the
	// status read happen while this session is busy. Released only here.
	if err := s.begin(); err != nil {
		return 0, err
	}
	closeAfterRelease := false
	defer func() {
		s.finish()
		if closeAfterRelease {
			e.finishClosing(context.WithoutCancel(ctx), s)
		}
	}()
	res, err := e.wireExecuteClaimed(ctx, s, sqlText, ip, &closeAfterRelease)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			// The TARGET refused: protocol data, in the client's own vocabulary.
			if eerr := emit(WireMessage{Kind: "ErrorResponse", Err: pgErr}); eerr != nil {
				return 0, eerr
			}
			return s.wireTxStatus()
		}
		return 0, err // the front door's own refusal; the loop frames it (§8a)
	}
	if res.More {
		return 0, ErrInterimTruncated
	}
	for _, m := range interimWireMessages(res) {
		if eerr := emit(m); eerr != nil {
			return 0, eerr
		}
	}
	return s.wireTxStatus()
}

// interimWireMessages re-encodes a decoded Result as text-format wire messages.
// INTERIM: no type information survives decoding, so every column is reported
// as text (OID 25, unbounded, text format) and every value is its text
// rendering. NULL stays NULL (nil); an empty string stays a zero-length
// non-nil value — the two are different on the wire and must stay different.
func interimWireMessages(res *Result) []WireMessage {
	var out []WireMessage
	if len(res.Columns) > 0 {
		fields := make([]WireField, len(res.Columns))
		for i, name := range res.Columns {
			fields[i] = WireField{Name: name, TypeOID: 25, TypeSize: -1, TypeModifier: -1, Format: 0}
		}
		out = append(out, WireMessage{Kind: "RowDescription", Fields: fields})
		for _, row := range res.Rows {
			vals := make([][]byte, len(row))
			for i, v := range row {
				vals[i] = interimTextValue(v)
			}
			out = append(out, WireMessage{Kind: "DataRow", Values: vals})
		}
		out = append(out, WireMessage{Kind: "CommandComplete", Tag: "SELECT " + strconv.Itoa(len(res.Rows))})
		return out
	}
	return append(out, WireMessage{Kind: "CommandComplete", Tag: interimCommandTag(res)})
}

// interimTextValue renders one decoded value the way the text protocol would.
func interimTextValue(v any) []byte {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		return append([]byte{}, x...)
	case string:
		return []byte(x)
	case bool:
		if x {
			return []byte("t")
		}
		return []byte("f")
	default:
		return []byte(fmt.Sprint(x))
	}
}

// interimCommandTag builds the tag PostgreSQL would have sent for a
// non-row-returning statement, from the verb and the affected count.
func interimCommandTag(res *Result) string {
	verb := strings.ToUpper(res.Verb)
	switch verb {
	case "INSERT":
		return "INSERT 0 " + strconv.FormatInt(res.Affected, 10)
	case "UPDATE", "DELETE", "MERGE", "COPY", "MOVE", "FETCH":
		return verb + " " + strconv.FormatInt(res.Affected, 10)
	case "":
		return "SELECT 0"
	}
	return verb
}
