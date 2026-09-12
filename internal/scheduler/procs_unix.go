//go:build !windows

package scheduler

import (
	"os/exec"
	"syscall"
)

// setNewPGroup puts the command in its own process group so the whole
// toolchain tree (sh → dart/flutter → compiler) can be signaled together
// (F5).
func setNewPGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// termGroup asks the command's process group to terminate, giving a
// cooperative engine the chance to restore the file it is mutating
// before killGroup escalates to SIGKILL.
func termGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

// killGroup kills the command's entire process group. With Setpgid, the
// group ID equals the leader's PID.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func groupRSS(pid int32) uint64 { return treeRSS(pid) }
