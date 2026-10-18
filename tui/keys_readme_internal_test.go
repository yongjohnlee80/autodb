package tui

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
)

var updateKeys = flag.Bool("update-keys", false, "rewrite README.md's table of key commands from the catalog")

// The README's key-command table, between these lines.
const (
	keysReadmeBegin = "<!-- the key commands: generated from tui/keys.go (go test ./tui -run TestTheReadmeListsEveryKeyCommand -update-keys) -->\n"
	keysReadmeEnd   = "<!-- end of the generated key commands -->\n"
)

// keysTable is every key command, its id, scope and default keys, a row each.
func keysTable() string {
	var b strings.Builder
	b.WriteString("| id | scope | keys |\n| --- | --- | --- |\n")
	for _, c := range keyCommands(catalogCommands()) {
		names := make([]string, len(c.Keys))
		for i, k := range c.Keys {
			names[i] = k.String()
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | %s |\n", c.ID, c.Scope, strings.Join(names, " "))
	}
	return b.String()
}

// TestTheReadmeListsEveryKeyCommand: README.md's table is every key command the program declares, with
// its default keys; -update-keys rewrites it.
func TestTheReadmeListsEveryKeyCommand(t *testing.T) {
	src, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	a, b := strings.Index(s, keysReadmeBegin), strings.Index(s, keysReadmeEnd)
	if a < 0 || b < a {
		t.Fatal("README.md lacks the key-command table's markers")
	}
	want := s[:a+len(keysReadmeBegin)] + keysTable() + s[b:]
	if *updateKeys {
		if err := os.WriteFile("../README.md", []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if want != s {
		t.Error("README.md's key commands are not the catalog's: go test ./tui -run TestTheReadmeListsEveryKeyCommand -update-keys")
	}
}
