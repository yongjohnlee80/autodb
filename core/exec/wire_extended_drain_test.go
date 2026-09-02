package exec

import (
	"context"
	"errors"
	"testing"

	golibpg "github.com/yongjohnlee80/golib/dao/postgres"

	"github.com/yongjohnlee80/golib/dao"
)

// THE PIPELINED DRAIN.
//
// These are helper-level cells on drainExtendedCounting, and they are honest
// about that: the live end-to-end proof against PostgreSQL is still owed. What
// they DO observe is the thing a live cell would find hardest to attribute — the
// termination model — because a client that pipelines is the normal case and a
// reader that stops early looks like a server that returned nothing.

// scriptedConn is a PinnedConn that replays a fixed message sequence.
type scriptedConn struct {
	msgs []golibpg.ExtendedMessage
	at   int
}

func (c *scriptedConn) Receive(context.Context) (golibpg.ExtendedMessage, error) {
	if c.at >= len(c.msgs) {
		// A drain that asks for more than the server sent is the defect this
		// stands in for: on a real wire it blocks until the context dies.
		return golibpg.ExtendedMessage{}, errors.New("scripted: drain read past the end of the response")
	}
	m := c.msgs[c.at]
	c.at++
	return m, nil
}

func (c *scriptedConn) Send(context.Context, golibpg.ExtendedOp) error { return nil }
func (c *scriptedConn) Flush(context.Context) error                    { return nil }
func (c *scriptedConn) Sync(context.Context) (byte, error)             { return 'I', nil }
func (c *scriptedConn) Release(context.Context) error                  { return nil }
func (c *scriptedConn) Discard()                                       {}
func (c *scriptedConn) BeginSessionTx(context.Context, dao.TxOptions) (dao.ContextTxConn, error) {
	return nil, errors.New("scripted: not used")
}

// collect drives the drain over `pending` wire-answered frames.
func collect(t *testing.T, script []golibpg.ExtendedMessage, pending int) ([]string, int64, error) {
	t.Helper()
	c := &scriptedConn{msgs: script}
	o := newExtObjects()
	for i := 0; i < pending; i++ {
		o.queueWire()
	}
	var got []string
	rows, err := drainExtendedCounting(context.Background(), c, o,
		func(m WireMessage) error { got = append(got, m.Kind); return nil }, nil)
	return got, rows, err
}

// A client pipelines Parse and Bind without flushing, and the Execute's Flush
// releases all three. The server answers ALL of them, in order, so the drain
// must read past the earlier answers to reach the result.
//
// This is the cell that catches a drain terminating on the first
// "terminal-looking" message: ParseComplete is the answer to a frame, and a
// reader that treats it as the end of the response abandons the rows behind it.
// The client sees a prepared statement and no result, which reads like an empty
// table rather than like a bug.
func TestExtDrain_PipelinedParseBindExecuteIsReadToTheEnd(t *testing.T) {
	script := []golibpg.ExtendedMessage{
		{Kind: "ParseComplete"},
		{Kind: "BindComplete"},
		{Kind: "DataRow", Values: [][]byte{[]byte("1")}},
		{Kind: "DataRow", Values: [][]byte{[]byte("2")}},
		{Kind: "CommandComplete", Tag: "SELECT 2"},
	}
	got, rows, err := collect(t, script, 3) // Parse + Bind + Execute outstanding
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	want := []string{"ParseComplete", "BindComplete", "DataRow", "DataRow", "CommandComplete"}
	if len(got) != len(want) {
		t.Fatalf("forwarded %v, want %v — the drain stopped before the result", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("forwarded %v, want %v", got, want)
		}
	}
	if rows != 2 {
		t.Errorf("rows = %d, want 2", rows)
	}
}

// Describe-statement answers with ParameterDescription AND THEN RowDescription,
// which is two messages for ONE frame. Counting the first would end the segment
// a message early and strip the row description off the front of the result.
func TestExtDrain_DescribeStatementCostsOneFrameNotTwoMessages(t *testing.T) {
	script := []golibpg.ExtendedMessage{
		{Kind: "ParseComplete"},
		{Kind: "ParameterDescription", ParameterOIDs: []uint32{23}},
		{Kind: "RowDescription", Fields: []golibpg.ExtendedFieldDescription{{Name: "n"}}},
	}
	got, _, err := collect(t, script, 2) // Parse + Describe outstanding
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	want := []string{"ParseComplete", "ParameterDescription", "RowDescription"}
	if len(got) != len(want) {
		t.Fatalf("forwarded %v, want %v — ParameterDescription was counted as its frame's answer", got, want)
	}
}

// An ErrorResponse voids everything still queued: PostgreSQL discards every
// frame but Sync and Terminate after one, so the answers the drain is still
// waiting for are never coming.
//
// The load-bearing half is that the drain RETURNS. A count that only decremented
// would sit waiting for two answers the server has already decided not to give,
// and on a real wire that is a hang, not a wrong result.
func TestExtDrain_AnErrorVoidsTheRestOfTheSegment(t *testing.T) {
	script := []golibpg.ExtendedMessage{
		{Kind: "ParseComplete"},
		{Kind: "ErrorResponse"},
		// Nothing follows: the server is discarding until Sync. A drain still
		// counting would read past the end and the scripted conn would say so.
	}
	got, _, err := collect(t, script, 4) // Parse + Bind + Execute + Close outstanding
	if err != nil {
		t.Fatalf("the drain kept waiting for answers the server will never send: %v", err)
	}
	want := []string{"ParseComplete", "ErrorResponse"}
	if len(got) != len(want) {
		t.Fatalf("forwarded %v, want %v", got, want)
	}
}

// The error is forwarded as protocol DATA, not turned into a Go error: it is the
// client's to read, and the front door does not get to decide what it was told.
func TestExtDrain_TheServerErrorReachesTheClientAsAFrame(t *testing.T) {
	script := []golibpg.ExtendedMessage{{Kind: "ErrorResponse"}}
	got, _, err := collect(t, script, 1)
	if err != nil {
		t.Fatalf("a server ErrorResponse became a Go error: %v", err)
	}
	if len(got) != 1 || got[0] != "ErrorResponse" {
		t.Fatalf("forwarded %v, want the ErrorResponse itself", got)
	}
}

// hostileConn refuses every wire operation, so a cell using it proves the code
// under test never touched the connection.
type hostileConn struct{ scriptedConn }

func (c *hostileConn) Receive(context.Context) (golibpg.ExtendedMessage, error) {
	return golibpg.ExtendedMessage{}, errors.New("the connection was read; owned control must never reach the wire")
}
func (c *hostileConn) Send(context.Context, golibpg.ExtendedOp) error {
	return errors.New("a frame was sent; owned control must never reach the wire")
}

// Owned transaction control is answered with the protocol's fixed shapes, in the
// order the client asked for them, WITHOUT touching the connection.
//
// The hostile connection is the load-bearing half: an assertion on the three
// frames alone would pass just as well for an implementation that relayed BEGIN
// to the server and got the same tag back — and that implementation is the one
// ADR-0018 r2 MF5 forbids, because the transaction it opens has no owner.
func TestExtDrain_OwnedControlIsAnsweredLocallyAndInOrder(t *testing.T) {
	o := newExtObjects()
	o.queueSynth(WireMessage{Kind: "ParseComplete"})
	o.queueSynth(WireMessage{Kind: "BindComplete"})
	o.queueSynth(WireMessage{Kind: "CommandComplete", Tag: "BEGIN"})

	var got []string
	var tag string
	_, err := drainExtendedCounting(context.Background(), &hostileConn{}, o,
		func(m WireMessage) error {
			got = append(got, m.Kind)
			if m.Kind == "CommandComplete" {
				tag = m.Tag
			}
			return nil
		}, nil)
	if err != nil {
		t.Fatalf("owned control touched the connection: %v", err)
	}
	want := []string{"ParseComplete", "BindComplete", "CommandComplete"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("forwarded %v, want %v", got, want)
		}
	}
	if tag != "BEGIN" {
		t.Errorf("CommandComplete tag = %q, want the owner's BEGIN", tag)
	}
}

// A Describe of a control statement answers ParameterDescription + NoData: no
// parameters, no rows. Both are one frame's answer, so the drain must not stop
// at the first.
func TestExtDrain_DescribeOfOwnedControlAnswersNoParamsAndNoRows(t *testing.T) {
	o := newExtObjects()
	o.queueSynth(WireMessage{Kind: "ParameterDescription"}, WireMessage{Kind: "NoData"})

	var got []string
	if _, err := drainExtendedCounting(context.Background(), &hostileConn{}, o,
		func(m WireMessage) error { got = append(got, m.Kind); return nil }, nil); err != nil {
		t.Fatalf("owned control touched the connection: %v", err)
	}
	if len(got) != 2 || got[0] != "ParameterDescription" || got[1] != "NoData" {
		t.Fatalf("forwarded %v, want [ParameterDescription NoData]", got)
	}
}
