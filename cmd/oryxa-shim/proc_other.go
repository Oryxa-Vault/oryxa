//go:build !unix

package main

import "os/exec"

// Process groups are a unix idea. Elsewhere a cancelled turn kills the process
// we started and nothing it started, which is worse but is not a reason to fail
// to build.
func setPGID(*exec.Cmd) {}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
