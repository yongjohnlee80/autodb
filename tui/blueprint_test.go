package tui_test

import (
	"io/fs"
	"os"
	"path"
	"testing"

	"github.com/yongjohnlee80/golib/parse/qml"
)

// TestEveryBlueprintFileParses: the blueprint — qml/blueprint/main.qml and the
// panels, views, dialogs and managers it imports — is the design every screen
// is built to. Until golib's vocabulary has what it uses (ADR-0197 §9), it is
// held to QML syntax: each file parses.
func TestEveryBlueprintFileParses(t *testing.T) {
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
		t.Fatalf("parsed %d QML files; the blueprint has more — is the walk rooted right?", n)
	}
}
