//go:build !windows

package manager

import (
	"os/exec"
	"syscall"
)

// setProcessGroup starts cmd in its own process group so the whole tree (npm
// and the node processes it starts) can be ended together.
func setProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// killProcessGroup ends cmd's process group.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
