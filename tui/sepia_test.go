// sepia_test.go — the copied theme must not drift from the pinned golib's
//: the constants compare against the toolkit's own file, so a
// golib re-tune surfaces as a diff here, not as silent divergence.

package tui_test

import (
	"io/fs"
	"strings"
	"testing"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
	"github.com/yongjohnlee80/golib/tui/decl/themes"
)

// TestSepiaMatchesThePinnedGolib — the copy equals the toolkit's file modulo
// the header (which names the origin and the one added key) and the one
// program-owned key the toolkit's groups lack.
func TestSepiaMatchesThePinnedGolib(t *testing.T) {
	ours, err := fs.ReadFile(tuiapp.CatalogFiles(), "themes/sepia.qml")
	if err != nil {
		t.Fatal(err)
	}
	src, err := fs.ReadFile(themes.FS(), "sepia.qml")
	if err != nil {
		t.Fatal(err)
	}
	// The comparison is over the Theme block: the constants the file ships.
	// Ours adds pressureRaisedText (this program's key); strip it, strip
	// comments and whitespace, and the two must read identically.
	block := func(b []byte) string {
		text := string(b)
		if i := strings.Index(text, "Theme {"); i >= 0 {
			text = text[i:]
		}
		var out strings.Builder
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "//") {
				continue
			}
			out.WriteString(line)
			out.WriteString("\n")
		}
		return out.String()
	}
	got := strings.ReplaceAll(block(ours), "pressureRaisedText: \"#b3402a\"\n", "")
	if got != block(src) {
		t.Errorf("themes/sepia.qml drifted from golib's; re-copy it from the pinned release and keep the header")
	}
}
