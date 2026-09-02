package frontdoor

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/yongjohnlee80/autodb/core/exec"
)

// THE DISCRIMINATOR: is the re-admission defect FIXED, or merely UNREACHABLE?
//
// The shape is ultron-prime's, and so is the reasoning that produced it. When
// his delivery boundary landed on #72 the existing discard-through-Sync cell
// went green — and he warned that a green integration cell is not evidence that
// re-admission is impossible: a boundary that keeps frames out of reach hides
// the defect rather than fixing it, and "fixed" and "unreachable" are different
// facts about a component.
//
// So this looks at what ONLY re-admission produces. PostgreSQL answers an error
// mid-segment by discarding until Sync and saying nothing further, so the count
// of ErrorResponses before readiness is the discriminator — one is conformant,
// many is the bug — and no reader-side change can alter that count either way.
//
// Measured on this base before the fix: four over-cap frames drew four refusals.
func TestAdmission_ADiscardingSegmentIsNotReAdmittedFrameByFrame(t *testing.T) {
	t.Parallel()
	q := okQueries()
	q.extMsgs = []exec.WireMessage{{Kind: "ParameterDescription"}, {Kind: "NoData"}}
	capBytes := int64(4096)
	capMsgs := 1 << 20 // out of the way: the BYTE cap is what trips here
	_, _, addr := listenerWith(t, Options{
		Authn: &fakeAuth{result: goodSession()}, Queries: q, AuthFailuresPerIP: unthrottled,
		testSegmentBytes: &capBytes, testSegmentMsgs: &capMsgs,
	})
	conn, fe := authenticated(t, addr)
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))

	// Four frames, each over the cap on its own, then Sync — in ONE write, so
	// they are pipelined behind the breach exactly as a real client sends them.
	big := rawDescribe(strings.Repeat("b", 5000))
	one := []byte{}
	for range 4 {
		one = append(one, big...)
	}
	if _, err := conn.Write(append(one, rawSync()...)); err != nil {
		t.Fatal(err)
	}

	refusals, sawReady := 0, false
	for !sawReady {
		m, err := fe.Receive()
		if err != nil {
			t.Fatalf("after %d refusals and no readiness: %v", refusals, err)
		}
		switch v := m.(type) {
		case *pgproto3.ErrorResponse:
			if v.Detail != ruleSegmentCap {
				t.Fatalf("refusal = %s/%s, want %s", v.Code, v.Detail, ruleSegmentCap)
			}
			refusals++
		case *pgproto3.ReadyForQuery:
			sawReady = true
		}
	}
	if refusals != 1 {
		t.Fatalf("%d ErrorResponses before readiness, want exactly 1 — PostgreSQL answers an error "+
			"mid-segment by discarding until Sync and saying nothing further, so one refusal per "+
			"over-cap frame means the segment is being RE-ADMITTED while already discarding", refusals)
	}
}

// ...and the same property at the UNIT, which is the half an integration test
// cannot be trusted for.
//
// Driving admitSegmentFrame directly is independent of what any reader delivers,
// so it separates "the component is correct" from "the component is currently
// unreachable" — the distinction the cell above cannot make on its own once a
// delivery boundary exists upstream.
func TestAdmission_AnAlreadyDiscardingSegmentIsNotAdmittedAgain(t *testing.T) {
	t.Parallel()
	l, conn, be, refusals := admissionHarness(t)
	closeReason := ""
	seg := &segmentLane{}
	over := frameHeader{typ: 'D', declared: int(maxSegmentBytes) + 1}

	if !l.admitSegmentFrame(conn, be, seg, over, "probe", &closeReason) {
		t.Fatal("the first breach closed the session; it must refuse the segment and survive")
	}
	if !seg.discarding {
		t.Fatal("the first breach did not set discarding; this cell cannot observe re-admission")
	}
	msgs, bytes := seg.msgs, seg.bytes

	if !l.admitSegmentFrame(conn, be, seg, over, "probe", &closeReason) {
		t.Fatal("the second frame closed the session")
	}
	if seg.msgs != msgs || seg.bytes != bytes {
		t.Errorf("a discarding segment was re-counted: msgs %d→%d, bytes %d→%d", msgs, seg.msgs, bytes, seg.bytes)
	}
	if got := refusals(); got != 1 {
		t.Errorf("the client received %d ErrorResponses for ONE segment breach, want 1", got)
	}
}

// admissionHarness gives a Listener, a live Backend, and a count of the
// ErrorResponses actually written to the peer — which is the assertion that
// matters, since a re-refusal is a defect precisely because the CLIENT sees it.
func admissionHarness(t *testing.T) (*Listener, net.Conn, *pgproto3.Backend, func() int) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	seen := make(chan int, 1)
	go func() {
		fe := pgproto3.NewFrontend(client, client)
		n := 0
		for {
			m, err := fe.Receive()
			if err != nil {
				seen <- n
				return
			}
			if _, ok := m.(*pgproto3.ErrorResponse); ok {
				n++
			}
		}
	}()

	l, _, _ := listenerWith(t, Options{
		Authn: &fakeAuth{result: goodSession()}, Queries: okQueries(), AuthFailuresPerIP: unthrottled,
	})
	return l, server, pgproto3.NewBackend(newFrameReader(server), server), func() int {
		_ = server.Close()
		_ = client.Close()
		select {
		case n := <-seen:
			return n
		case <-time.After(5 * time.Second):
			t.Fatal("the frontend reader did not finish")
			return -1
		}
	}
}
