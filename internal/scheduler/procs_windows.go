//go:build windows

package scheduler

import (
	"os/exec"
)

// Windows has no process groups; fall back to signaling only the direct
// child. Timeouts still kill the shell, though grandchildren may linger.

func setNewPGroup(*exec.Cmd) {}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func groupRSS(pid int32) uint64 { return treeRSS(pid) }
