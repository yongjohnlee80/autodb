package frontdoor

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// rawQuery builds a complete Query frame, for cells that need bytes pgproto3
// will not send on their terms.
func rawQuery(sql string) []byte {
	b := make([]byte, 5, 5+len(sql)+1)
	b[0] = 'Q'
	binary.BigEndian.PutUint32(b[1:], uint32(4+len(sql)+1))
	b = append(b, sql...)
	return append(b, 0)
}

// noOfferOverAWonFrame is the invariant both cells below assert.
func noOfferOverAWonFrame(t *testing.T, d *demandEngine) (sawFramed, sawMid bool) {
	t.Helper()
	for i, dec := range d.offerDecisions() {
		if dec.framed {
			sawFramed = true
		}
		if dec.mid {
			sawMid = true
		}
		if (dec.framed || dec.mid) && dec.offered {
			t.Errorf("pass %d published a receive offer with framed=%v mid=%v; the client "+
				"had already won that pass, so demand could reserve the session inside the "+
				"offer window and discard a frame that was never ours to discard",
				i, dec.framed, dec.mid)
		}
	}
	return sawFramed, sawMid
}

// AN OFFER IS NEVER PUBLISHED OVER A HEADER THAT IS ALREADY FRAMED.
//
// The loop used to offer unconditionally, which looks right because the offer
// sits immediately before the blocking read. But waitHeader does not always
// block: it returns at once when a header is already pending, and in that pass
// the client had already won before the offer existed.
//
// REACHING THAT STATE TOOK FINDING OUT WHERE IT COMES FROM. The reader takes a
// header one byte at a time, and every read after a frame is admitted is
// bounded to that frame, so a running session cannot over-read into the next
// message -- an earlier version of this cell tried to provoke it by pipelining
// and correctly reported that it had tested nothing. The one unbounded read is
// the auth exchange's, before any frame is admitted: a client that pipelines
// its first query with its last authentication message leaves that query's
// header framed and pending when the session loop starts. That is the
// precondition built here.
func TestDemandOfferWindow_NoOfferCoversAFrameTheClientAlreadyWon(t *testing.T) {
	eng := newDemandEngine()
	eng.preload = rawQuery("SELECT 1")
	fe, d, _, wait := drivenSession(t, eng)

	served := false
	for range 16 {
		msg, err := fe.Receive()
		if err != nil {
			break
		}
		if e, ok := msg.(*pgproto3.ErrorResponse); ok && e.Severity == "FATAL" {
			t.Fatalf("the client was terminated although its own frame had already been "+
				"framed before the loop ever offered: %s", e.Message)
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			served = true
			break
		}
	}
	if !served {
		t.Fatalf("the already-framed query was never answered%s", d.whyLoopEnded())
	}
	if n := d.finishCount(); n != 0 {
		t.Errorf("finalisation ran %d times for a session nobody reclaimed, want 0", n)
	}

	fe.Send(&pgproto3.Terminate{})
	_ = fe.Flush()
	wait()

	sawFramed, _ := noOfferOverAWonFrame(t, d)
	// WITHOUT THIS THE CELL IS VACUOUS, and vacuity is exactly how the older
	// "a frame that wins" cell stayed green over this defect: an ordinary
	// client never produces a pre-framed header, so a run that never reached
	// the state would pass while proving nothing about it.
	if !sawFramed {
		t.Fatalf("no pass ever saw a pre-framed header, so nothing was tested; "+
			"%d decisions recorded", len(d.offerDecisions()))
	}
}

// A BODY STILL OUTSTANDING IS ALSO A FRAME THE CLIENT HAS WON.
//
// midMessage is true for the whole of a message, not only for a split header:
// once a header is framed the reader is in its body phase until the last byte
// arrives. A client that sends its header and then stalls -- a slow network, a
// large parameter still being written -- is part-way through a message it has
// already begun, and an offer published over it promises something untrue.
//
// Driven the same way as the cell above, because it has the same origin: the
// auth exchange's unbounded read frames the header, and the body is still in
// flight when the session loop makes its first decision.
func TestDemandOfferWindow_NoOfferCoversABodyStillArriving(t *testing.T) {
	const body = "SELECT 1"
	head := make([]byte, 5)
	head[0] = 'Q'
	binary.BigEndian.PutUint32(head[1:], uint32(4+len(body)+1))

	eng := newDemandEngine()
	eng.preload = head // the header alone; the body follows below
	fe, d, _, wait := drivenSession(t, eng)

	// The loop is now in its body phase with nothing left to read. Let it
	// reach its decision, then finish the frame.
	time.Sleep(100 * time.Millisecond)
	if _, err := d.clientConn.Write(append([]byte(body), 0)); err != nil {
		t.Fatal(err)
	}

	served := false
	for range 16 {
		msg, err := fe.Receive()
		if err != nil {
			break
		}
		if e, ok := msg.(*pgproto3.ErrorResponse); ok && e.Severity == "FATAL" {
			t.Fatalf("the client was terminated with its own message part-delivered: %s",
				e.Message)
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			served = true
			break
		}
	}
	if !served {
		t.Fatalf("the part-delivered frame was never served%s", d.whyLoopEnded())
	}
	if n := d.finishCount(); n != 0 {
		t.Errorf("finalisation ran %d times for a session nobody reclaimed, want 0", n)
	}

	fe.Send(&pgproto3.Terminate{})
	_ = fe.Flush()
	wait()

	if _, sawMid := noOfferOverAWonFrame(t, d); !sawMid {
		t.Fatalf("no pass ever saw a message in progress, so nothing was tested; "+
			"%d decisions recorded", len(d.offerDecisions()))
	}
}
