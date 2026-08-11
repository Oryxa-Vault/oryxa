//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// setPGID puts the command in its own process group, so everything it starts
// can be killed together.
func setPGID(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// A negative pid addresses the group. If the group is gone — the child
	// already exited and took it with it — fall back to the process, which is
	// also the right call on the paths where Setpgid did not take effect.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
