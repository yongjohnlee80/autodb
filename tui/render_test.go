package tui

import "testing"

// The FRONTEND owns display: the server ships bytes, and how they read
// is decided here (Johno, M6 manual testing).
func TestBytesTextDisplayRule(t *testing.T) {
	uuid := []byte{0x05, 0xc1, 0x58, 0x6d, 0x67, 0xda, 0x43, 0x06,
		0x9b, 0x3f, 0xbb, 0x7b, 0xec, 0x28, 0xba, 0xd0}
	if got, want := bytesText(uuid), "05c1586d-67da-4306-9b3f-bb7bec28bad0"; got != want {
		t.Errorf("uuid: got %q, want %q", got, want)
	}
	// mysql hands back every string column as []byte.
	if got, want := bytesText([]byte("Vancouver")), "Vancouver"; got != want {
		t.Errorf("text: got %q, want %q", got, want)
	}
	if got, want := bytesText([]byte("multi\nline\ttabbed")), "multi\nline\ttabbed"; got != want {
		t.Errorf("whitespace text: got %q, want %q", got, want)
	}
	// Genuine binary reads as hex.
	if got, want := bytesText([]byte{0x00, 0x01, 0xff}), "0x0001ff"; got != want {
		t.Errorf("binary: got %q, want %q", got, want)
	}
	if got, want := bytesText([]byte{0xff, 0xfe}), "0xfffe"; got != want {
		t.Errorf("invalid utf-8: got %q, want %q", got, want)
	}
	// Table cells stay single-line; newlines are shown as ␤.
	if got, want := renderCell([]byte("a\nb")), "a␤b"; got != want {
		t.Errorf("cell newline: got %q, want %q", got, want)
	}
	if got, want := renderCell(nil), "NULL"; got != want {
		t.Errorf("nil: got %q, want %q", got, want)
	}
	// The register copy keeps the value faithful (real newline).
	if got, want := faithfulCell([]byte("a\nb")), "a\nb"; got != want {
		t.Errorf("faithful newline: got %q, want %q", got, want)
	}
	if got, want := faithfulCell(uuid), "05c1586d-67da-4306-9b3f-bb7bec28bad0"; got != want {
		t.Errorf("faithful uuid: got %q, want %q", got, want)
	}
}
