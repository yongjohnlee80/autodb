package rpc

import (
	"net"
	"testing"
	"time"
)

// A postgres uuid scans into [16]uint8 — a fixed-size ARRAY, which the
// []byte case never matches. Before this normalization it reached the
// frontends through %v as a decimal byte list.
func TestWireValNormalizesByteArraysAndTextTypes(t *testing.T) {
	uuid := [16]byte{0x05, 0xc1, 0x58, 0x6d, 0x67, 0xda, 0x43, 0x06,
		0x9b, 0x3f, 0xbb, 0x7b, 0xec, 0x28, 0xba, 0xd0}
	if got, want := wireVal(uuid), "05c1586d-67da-4306-9b3f-bb7bec28bad0"; got != want {
		t.Errorf("uuid: got %v, want %q", got, want)
	}
	if got, want := wireVal([4]byte{0xde, 0xad, 0xbe, 0xef}), "deadbeef"; got != want {
		t.Errorf("byte array: got %v, want %q", got, want)
	}
	// Bytes that ARE text ship as text (mysql returns every string column
	// as []byte; postgres bytea holding text likewise).
	if got, want := wireVal([]byte("hello")), "hello"; got != want {
		t.Errorf("text bytes: got %#v, want %q", got, want)
	}
	if got, want := wireVal([]byte("multi\nline\ttabbed")), "multi\nline\ttabbed"; got != want {
		t.Errorf("text bytes with whitespace: got %#v, want %q", got, want)
	}
	// Genuine binary stays binary: the frontends render []byte as 0x hex.
	if got, ok := wireVal([]byte{0x00, 0x01, 0xff}).([]byte); !ok || len(got) != 3 {
		t.Errorf("binary should pass through, got %#v", wireVal([]byte{0x00, 0x01, 0xff}))
	}
	if got, ok := wireVal([]byte{0xff, 0xfe}).([]byte); !ok || len(got) != 2 {
		t.Errorf("invalid utf-8 should pass through, got %#v", wireVal([]byte{0xff, 0xfe}))
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
