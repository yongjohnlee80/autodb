package tui

import (
	"fmt"
	"os"
	"time"

	tuicore "github.com/yongjohnlee80/golib/tui"
)

// RuntimeTrace is golib's runtime tracer, writing to the file AUTODB_FOCUS_TRACE
// names: every focus move with the component types on both ends, every repair
// and why it ran, every scope opening and closing, and the node that consumed
// each key. Nil when the variable is unset, which disables tracing.
//
// The file is opened once and kept: the tracer runs on the loop for every event
// and must stay cheap. It lives as long as the process does.
func RuntimeTrace() tuicore.TraceFunc {
	path := os.Getenv("AUTODB_FOCUS_TRACE")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil
	}
	return func(ev tuicore.TraceEvent) {
		fmt.Fprintf(f, "%s golib %-12s node=%d(%s) prev=%d(%s) %s\n",
			time.Now().Format("15:04:05.000"), ev.Kind, ev.Node, ev.Comp, ev.Prev, ev.PrevComp, ev.Detail)
	}
}
