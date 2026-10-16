//go:build !unix

package autodb

import "os/exec"

// detach does nothing where there are no sessions to start one in.
func detach(*exec.Cmd) {}
