package frontdoor

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgproto3"
)

// successSequenceWithNotices drives auth to ReadyForQuery and returns every
// ParameterStatus and NoticeResponse the server sent on the way.
func successSequenceWithNotices(t *testing.T, addr string, params map[string]string) (map[string]string, []*pgproto3.NoticeResponse) {
	t.Helper()
	_, fe := startupTo(t, addr, params)
	if _, err := fe.Receive(); err != nil {
		t.Fatalf("auth request: %v", err)
	}
	fe.Send(&pgproto3.PasswordMessage{Password: "autodb_pat_secret"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	var notices []*pgproto3.NoticeResponse
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("success sequence: %v", err)
		}
		switch m := msg.(type) {
		case *pgproto3.ParameterStatus:
			statuses[m.Name] = m.Value
		case *pgproto3.NoticeResponse:
			notices = append(notices, &pgproto3.NoticeResponse{Code: m.Code, Message: m.Message})
		case *pgproto3.ReadyForQuery:
			return statuses, notices
		}
	}
}

// An application_name over 256 bytes is truncated on a rune boundary, the peer
// is told by a NoticeResponse, and the VERBATIM original is audited (matrix
// row 3.1:application_name#truncate-notice-256).
func TestStartup_ApplicationNameIsCappedAt256Bytes(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	events, addr := authListener(t, f)

	// 300 ASCII bytes with a tail that cannot survive truncation.
	long := strings.Repeat("a", 290) + "-THE-TAIL-"
	params := defaultParams()
	params["application_name"] = long
	statuses, notices := successSequenceWithNotices(t, addr, params)

	got := statuses["application_name"]
	if len(got) != applicationNameMaxBytes {
		t.Fatalf("echoed application_name is %d bytes, want exactly %d", len(got), applicationNameMaxBytes)
	}
	if got != long[:applicationNameMaxBytes] {
		t.Fatalf("echoed value is not the 256-byte prefix of what was sent")
	}
	if strings.Contains(got, "THE-TAIL") {
		t.Fatal("the tail survived truncation")
	}
	var sawNotice bool
	for _, n := range notices {
		if strings.Contains(n.Message, "256") && strings.Contains(n.Message, "truncated") {
			sawNotice = true
		}
	}
	if !sawNotice {
		t.Fatalf("no *pgproto3.NoticeResponse announcing the truncation; got %d notice(s)", len(notices))
	}
	var audited string
	for _, ev := range events() {
		if ev.Kind == "fd.param_truncated" {
			audited = ev.Detail
		}
	}
	if audited != long {
		t.Fatalf("fd.param_truncated must carry the VERBATIM original (%d bytes); got %d bytes", len(long), len(audited))
	}
}

// Truncation never splits a rune: a multi-byte application_name comes back
// valid UTF-8 and at most 256 bytes.
func TestStartup_ApplicationNameTruncationIsRuneSafe(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	_, addr := authListener(t, f)

	params := defaultParams()
	// A 3-byte rune: 256 is not a multiple of 3, so byte 256 falls MID-RUNE and
	// the cut must back off to 255. (A 2-byte rune would not exercise this —
	// 256 is even and lands cleanly on a boundary.)
	params["application_name"] = strings.Repeat("€", 100) // 300 bytes, 3 per rune
	statuses, _ := successSequenceWithNotices(t, addr, params)
	got := statuses["application_name"]
	if len(got) > applicationNameMaxBytes {
		t.Fatalf("truncated value is %d bytes, over the %d cap", len(got), applicationNameMaxBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncation split a rune: the echoed application_name is not valid UTF-8")
	}
	if len(got) != 255 { // 85 whole runes × 3 bytes; 256 would split the 86th
		t.Fatalf("expected the rune boundary below 256 (255 bytes), got %d", len(got))
	}
}

// A short application_name is neither truncated, noticed, nor audited as such —
// the negative control that shows the cells above observe the CONDITION.
func TestStartup_ShortApplicationNameIsLeftAlone(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	events, addr := authListener(t, f)

	params := defaultParams()
	params["application_name"] = "psql"
	statuses, notices := successSequenceWithNotices(t, addr, params)
	if statuses["application_name"] != "psql" {
		t.Fatalf("short application_name changed to %q", statuses["application_name"])
	}
	if len(notices) != 0 {
		t.Fatalf("a short application_name produced %d notice(s)", len(notices))
	}
	for _, ev := range events() {
		if ev.Kind == "fd.param_truncated" {
			t.Fatal("fd.param_truncated audited for a value under the cap")
		}
	}
}

// An empty/whitespace options is accepted, ignored, and AUDITED — and a startup
// with no options key at all emits no such event (matrix row 3.1:options#empty-audit).
func TestStartup_EmptyOptionsIsAuditedAsIgnored(t *testing.T) {
	t.Parallel()
	f := &fakeAuth{result: goodSession()}
	events, addr := authListener(t, f)

	params := defaultParams()
	params["options"] = "   "
	successSequenceWithNotices(t, addr, params)
	var seen int
	for _, ev := range events() {
		if ev.Kind == "fd.param_ignored" && ev.Detail == "options" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("fd.param_ignored for the empty options: got %d event(s), want exactly 1", seen)
	}

	// Negative control: no options key at all → nothing to ignore, nothing audited.
	f2 := &fakeAuth{result: goodSession()}
	events2, addr2 := authListener(t, f2)
	successSequenceWithNotices(t, addr2, defaultParams())
	for _, ev := range events2() {
		if ev.Kind == "fd.param_ignored" {
			t.Fatal("fd.param_ignored emitted for a startup with no options key at all")
		}
	}
}
