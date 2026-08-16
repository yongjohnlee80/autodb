package tui

import (
	"strings"
	"testing"
)

// The ID grammar's only structural byte is ':' — encSeg must remove it
// from every free-text segment (url.PathEscape deliberately does NOT),
// and decSeg must reverse the encoding exactly.
func TestSegmentEncodingRoundTrip(t *testing.T) {
	cases := []string{
		"plain",
		"a:b",
		"a:b:c",
		"%3A",       // literal text that LOOKS pre-encoded must survive
		"100%:done", // percent and colon together
		"sp ace.and-more_",
		"schema.tbl",
	}
	for _, in := range cases {
		enc := encSeg(in)
		if strings.ContainsRune(enc, ':') {
			t.Errorf("encSeg(%q) = %q still contains ':'", in, enc)
		}
		if got := decSeg(enc); got != in {
			t.Errorf("decSeg(encSeg(%q)) = %q", in, got)
		}
	}
}
