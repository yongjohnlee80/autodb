package rpc

import (
	"net"
	"testing"
	"time"
)

// wireVal carries values the codec can encode; it does NOT format them
// for display. A postgres uuid scans into [16]uint8 — a fixed-size
// ARRAY the []byte case never matches — and reached the frontends
// through %v as a decimal byte list before this normalization.
func TestWireValCarriesBytesAndTextTypes(t *testing.T) {
	// A fixed-size byte array (postgres uuid → [16]uint8) carries the SAME
	// BYTES onto the wire: the server ships values, the frontend decides
	// how they read. Before this, %v shipped a decimal byte list.
	uuid := [16]byte{0x05, 0xc1, 0x58, 0x6d, 0x67, 0xda, 0x43, 0x06,
		0x9b, 0x3f, 0xbb, 0x7b, 0xec, 0x28, 0xba, 0xd0}
	got, ok := wireVal(uuid).([]byte)
	if !ok || len(got) != 16 || got[0] != 0x05 || got[15] != 0xd0 {
		t.Errorf("uuid array: got %#v, want the 16 raw bytes", wireVal(uuid))
	}
	// []byte passes through untouched — text-vs-hex is a display choice.
	if got, ok := wireVal([]byte("hello")).([]byte); !ok || string(got) != "hello" {
		t.Errorf("text bytes must pass through, got %#v", wireVal([]byte("hello")))
	}
	if got, ok := wireVal([]byte{0x00, 0x01, 0xff}).([]byte); !ok || len(got) != 3 {
		t.Errorf("binary must pass through, got %#v", wireVal([]byte{0x00, 0x01, 0xff}))
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
