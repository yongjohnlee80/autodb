package autodocplugin

import (
	"errors"
	"fmt"
	"strings"

	"github.com/yongjohnlee80/autodb/core/engine"
	"github.com/yongjohnlee80/autodoc/plugin"
)

// errNoStatement is a note, or a cursor, with nothing to run: only space or comments.
var errNoStatement = errors.New("there is no statement here to run: the note holds only space or comments")

// statementAt is what "run" sends: the selection's bytes when there is a selection, else the
// statement holding the cursor's byte offset.
//
// The statement is found with the engine's own splitter (engine.Name.StatementSplitter), built
// from the same lexical rules as the gate AutoDB classifies a statement with, so it begins where
// AutoDB begins one. A cursor inside a comment finds the statement that holds the comment, never
// the commented text; a note of only comments finds none, and nothing is sent.
func statementAt(d plugin.Document, eng engine.Name) (string, error) {
	text := d.Text
	for _, r := range d.SelectionBytes {
		from, to := r[0], r[1]
		if from < 0 || to > len(text) || from >= to {
			continue
		}
		sel := strings.TrimSpace(text[from:to])
		if sel == "" {
			return "", errors.New("the selection holds only space")
		}
		return sel, nil
	}
	at := min(max(d.CursorByte, 0), len(text))
	st, ok, err := eng.StatementSplitter().StatementAt([]byte(text), at)
	if err != nil {
		return "", fmt.Errorf("the statement at the cursor cannot be read: %v", err)
	}
	if !ok {
		return "", errNoStatement
	}
	return st.Text, nil
}
