//go:build !windows

package client

import (
	"os/exec"
	"syscall"
)

// detach starts molt-swap in its own session so it outlives the app.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
