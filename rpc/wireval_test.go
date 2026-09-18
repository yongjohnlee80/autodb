package rpc

import (
	"net"
	"testing"
	"time"
)

// wireVal carries values the codec can encode. It formats exactly one thing:
// a 16-byte fixed-size ARRAY, which is how a postgres uuid arrives. Everything
// else is carried, not rendered.
func TestWireValCarriesBytesAndTextTypes(t *testing.T) {
	// A uuid scans into [16]uint8 — a fixed-size ARRAY the []byte case never
	// matches — while a bytea arrives as a []byte SLICE. Those are distinct Go
	// types HERE and indistinguishable once both are bytes on the wire, so the
	// canonical text form is decided here, at the last point the type still
	// exists. Previously this shipped raw bytes and every frontend without type
	// information rendered them as 0x-prefixed hex.
	uuid := [16]byte{0x05, 0xc1, 0x58, 0x6d, 0x67, 0xda, 0x43, 0x06,
		0x9b, 0x3f, 0xbb, 0x7b, 0xec, 0x28, 0xba, 0xd0}
	if got, want := wireVal(uuid), "05c1586d-67da-4306-9b3f-bb7bec28bad0"; got != want {
		t.Errorf("uuid array: got %#v, want %q", got, want)
	}
	// []byte passes through untouched — text-vs-hex is a display choice.
	if got, ok := wireVal([]byte("hello")).([]byte); !ok || string(got) != "hello" {
		t.Errorf("text bytes must pass through, got %#v", wireVal([]byte("hello")))
	}
	if got, ok := wireVal([]byte{0x00, 0x01, 0xff}).([]byte); !ok || len(got) != 3 {
		t.Errorf("binary must pass through, got %#v", wireVal([]byte{0x00, 0x01, 0xff}))
	}
	// A 16-byte bytea is a SLICE, not an array, so it must NOT acquire uuid
	// formatting. This is the case a renderer-side len(b)==16 heuristic gets
	// wrong, and the reason the decision belongs here and not at the frontend.
	sixteen := []byte{0x05, 0xc1, 0x58, 0x6d, 0x67, 0xda, 0x43, 0x06,
		0x9b, 0x3f, 0xbb, 0x7b, 0xec, 0x28, 0xba, 0xd0}
	if got, ok := wireVal(sixteen).([]byte); !ok || len(got) != 16 {
		t.Errorf("16-byte bytea must stay bytes, got %#v", wireVal(sixteen))
	}
	// GUARD THE PREMISE. The formatting rests on "only uuid scans into a
	// fixed-size byte array" — a property of pgx, not of our code. Length is
	// therefore part of the match: a fixed array of any OTHER size keeps the
	// byte passthrough rather than being mislabelled as a uuid, so a future
	// codec returning [8]byte or [32]byte is visibly hex, never a wrong uuid.
	if got, ok := wireVal([8]byte{1, 2, 3, 4, 5, 6, 7, 8}).([]byte); !ok || len(got) != 8 {
		t.Errorf("non-16 byte array must not be uuid-formatted, got %#v",
			wireVal([8]byte{1, 2, 3, 4, 5, 6, 7, 8}))
	}
	// Stringer / TextMarshaler beat the %v struct dump.
	if got, want := wireVal(net.ParseIP("10.0.0.1")), "10.0.0.1"; got != want {
		t.Errorf("net.IP: got %v, want %q", got, want)
	}
	ts := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	if got, want := wireVal(ts), "2026-08-16T12:00:00Z"; got != want {
		t.Errorf("time: got %v, want %q", got, want)
	}
	// Scalars and nil are untouched.
	if got := wireVal(int64(7)); got != int64(7) {
		t.Errorf("int64: got %#v", got)
	}
	if got := wireVal(nil); got != nil {
		t.Errorf("nil: got %#v", got)
	}
}
