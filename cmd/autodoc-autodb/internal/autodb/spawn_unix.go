//go:build unix

package autodb

import (
	"os/exec"
	"syscall"
)

// detach starts cmd in a session of its own, so the server outlives the plugin and its process
// group.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
