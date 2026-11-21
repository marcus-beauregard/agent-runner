//go:build unix

package tools

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup gives cmd its own process group, so one kill reaches every
// child it starts, and makes cancellation kill that group rather than the
// process alone. A child that calls setsid leaves the group; catching it is the
// container sandbox's job (PRD §6.2), not this file's.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
}

// killProcessGroup sends SIGKILL to cmd's process group. It is safe after the
// leader has exited: the group outlives it while any child is still running.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

// exitStatus reads the code and signal out of a finished process. A process
// killed by a signal has no exit code, so it reports -1 and the signal's name.
func exitStatus(state *os.ProcessState) (int, string) {
	if state == nil {
		return -1, ""
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -1, ws.Signal().String()
	}
	return state.ExitCode(), ""
}
