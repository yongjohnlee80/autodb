package tui_test

import (
	"io/fs"
	"os"
	"path"
	"testing"

	"github.com/yongjohnlee80/golib/parse/qml"
)

// TestEveryQMLFileParses: every QML file the program embeds — main.qml, its
// panels, views, dialogs and managers, and the themes — is valid QML syntax.
// A file that fails to parse would otherwise surface only when the screen that
// imports it is first opened.
func TestEveryQMLFileParses(t *testing.T) {
	root := os.DirFS("qml")
	n := 0
	err := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".qml" {
			return err
		}
		src, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		if _, err := (qml.QML{File: p}).Parse(src); err != nil {
			t.Errorf("%s: %v", p, err)
		}
		n++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 30 {
		t.Fatalf("parsed %d QML files; the program embeds more — is the walk rooted right?", n)
	}
}
