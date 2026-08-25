package tui

import tui "github.com/yongjohnlee80/golib/tui"

// taskResultOf wraps a value the way the runtime delivers a completed task.
func taskResultOf(v any) tui.TaskResult { return tui.TaskResult{Value: v} }
