package tui

import "time"

func deadlineFrom() time.Time         { return time.Now().Add(5 * time.Second) }
func beforeDeadline(d time.Time) bool { return time.Now().Before(d) }
func sleepTick()                      { time.Sleep(20 * time.Millisecond) }
