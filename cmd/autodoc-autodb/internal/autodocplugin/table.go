package autodocplugin

import (
	"fmt"
	"sort"
	"unicode"
	"unicode/utf8"

	"github.com/yongjohnlee80/autodoc/plugin"
)

// cellText is one wire value as a table cell, by the rule AutoDB's own results use: NULL for no
// value, bytes as text when they read as text, as a UUID when they are 16, else as hex.
func cellText(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case string:
		return x
	case []byte:
		return bytesText(x)
	default:
		return fmt.Sprint(x)
	}
}

func bytesText(b []byte) string {
	if printable(b) {
		return string(b)
	}
	if len(b) == 16 {
		return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	}
	return fmt.Sprintf("0x%x", b)
}

func printable(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, r := range string(b) {
		if r != '\t' && r != '\n' && r != '\r' && unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// textRows are the wire rows as cells.
func textRows(rows [][]any) [][]string {
	out := make([][]string, len(rows))
	for i, r := range rows {
		cells := make([]string, len(r))
		for j, v := range r {
			cells[j] = cellText(v)
		}
		out[i] = cells
	}
	return out
}

// maxMessage is the link's bound on one message; a test lowers it.
var maxMessage = plugin.MaxMessageBytes

// fit is how many of rows, from the first, a message built by build holds within the link's
// bound: the largest k with build(rows[:k]) no larger than maxMessage. When not even the first
// row fits, its cells are cut until it does, and fit answers 1 with the row cut in place.
func fit(rows [][]string, build func([][]string) []any, method string) int {
	size := func(k int) int {
		n, err := plugin.EncodedSize(method, build(rows[:k]))
		if err != nil {
			return maxMessage + 1
		}
		return n
	}
	if len(rows) == 0 || size(len(rows)) <= maxMessage {
		return len(rows)
	}
	// size grows with k: the first k that does not fit, less one
	k := sort.Search(len(rows)+1, func(k int) bool { return size(k) > maxMessage }) - 1
	if k >= 1 {
		return k
	}
	cutRow(rows[0], func() bool { return size(1) <= maxMessage })
	return 1
}

// cutRow halves the longest cells of row, each ending in a mark saying it was cut, until fits.
func cutRow(row []string, fits func() bool) {
	limit := 0
	for _, c := range row {
		limit = max(limit, len(c))
	}
	for !fits() && limit > 0 {
		limit /= 2
		for i, c := range row {
			if len(c) > limit {
				row[i] = cut(c, limit)
			}
		}
	}
}

// cut is s within n bytes, on a rune boundary, with a mark that it was cut.
func cut(s string, n int) string {
	const mark = "…[cut]"
	if n <= len(mark) {
		return mark
	}
	end := n - len(mark)
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + mark
}
