package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yongjohnlee80/golib/logger"
	tuicore "github.com/yongjohnlee80/golib/tui"
)

// THE INSTRUMENT IS PROVEN TO OBSERVE BEFORE IT IS TRUSTED.
//
// RuntimeTrace exists to catch a focus transition nobody has been able to
// name from the code. A tracer that silently wrote nothing would report the
// production defect as "no transitions", which is indistinguishable from the
// defect having gone away. So this drives one focus move through a real App
// with the tracer attached and requires the file to name BOTH ends of it by
// component type -- the explorer gaining, whatever held it before losing.
func TestRuntimeTrace_NamesTheComponentsOnBothEndsOfAFocusMove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "focus.log")
	t.Setenv("AUTODB_FOCUS_TRACE", path)
	trace := RuntimeTrace()
	if trace == nil {
		t.Fatal("RuntimeTrace returned nil with AUTODB_FOCUS_TRACE set")
	}

	sess := NewSessionOn("tcp", "127.0.0.1:1", logger.Nop{}, nil)
	sess.user = UserInfo{ID: 1, Name: "op", Role: "admin"}
	m := New(sess, nil, nil)
	m.splashShown = true
	tb := tuicore.NewTestBackend(100, 30)
	app := tuicore.NewApp(m.Root(), tuicore.WithBackend(tb),
		tuicore.WithMinFrameInterval(0), tuicore.WithTrace(trace))
	h := &barHarness{t: t, m: m, tb: tb, app: app, done: make(chan error, 1)}
	go func() { h.done <- app.Run(t.Context()) }()
	t.Cleanup(func() {
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			t.Error("the app never exited")
		}
	})
	h.settle()

	h.on(func() { h.m.focusPane(h.m.editor) })
	h.settle()
	h.on(func() { h.m.focusPane(h.m.explorer) })
	h.settle()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the trace file was never written: %v", err)
	}
	var moveToExplorer string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "golib focus ") && strings.Contains(line, "node=") &&
			strings.Contains(line, "(*tui.explorer)") && strings.Contains(line, "prev=") {
			moveToExplorer = line
		}
	}
	if moveToExplorer == "" {
		t.Fatalf("no 'golib focus' line names *tui.explorer as the gainer; trace was:\n%s", raw)
	}
	// THE LOSER IS NAMED TOO. The production question is who focus was taken
	// FROM when the panes report nothing; a line that named only the gainer
	// could not answer it.
	if !strings.Contains(moveToExplorer, "prev=") || strings.Contains(moveToExplorer, "prev=0(") {
		t.Errorf("the move to the explorer does not name what lost focus: %s", moveToExplorer)
	}
	if !strings.Contains(moveToExplorer, "(*widget.Editor)") {
		t.Errorf("the editor held focus before the move and is not named as the loser: %s", moveToExplorer)
	}
}

// UNSET MEANS OFF, and off means nil -- which is what tui.WithTrace treats as
// disabled. Returning a func that writes nowhere would cost every event on the
// loop for no reader.
func TestRuntimeTrace_IsNilWhenTheVariableIsUnset(t *testing.T) {
	t.Setenv("AUTODB_FOCUS_TRACE", "")
	if RuntimeTrace() != nil {
		t.Fatal("RuntimeTrace returned a tracer with AUTODB_FOCUS_TRACE unset")
	}
}
