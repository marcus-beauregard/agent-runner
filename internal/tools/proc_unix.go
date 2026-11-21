//go:build unix

package tools

import (
	"errors"
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
	cmd.Cancel = func() error {
		_, err := killProcessGroup(cmd)
		return err
	}
}

// killProcessGroup sends SIGKILL to cmd's process group and reports whether
// the group still had a member to receive it. It is safe after the leader has
// exited: the group outlives it while any child is still running. An empty
// group answers ESRCH, which is not an error here — it means nothing was left
// behind to kill.
func killProcessGroup(cmd *exec.Cmd) (bool, error) {
	if cmd.Process == nil {
		return false, nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
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
