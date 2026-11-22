//go:build unix

package gate

import (
	"errors"
	"os/exec"
	"syscall"
)

// setProcessGroup puts the gate's shell in its own process group, so a child it
// starts can be killed as a group and a cancelled run reaches the whole tree
// rather than the shell alone.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		_, err := killProcessGroup(cmd)
		return err
	}
}

// killProcessGroup sends SIGKILL to the gate's process group and reports
// whether the group still had a member to receive it. It is safe after the
// shell has exited: the group outlives it while any child is still running. An
// empty group answers ESRCH, which is not an error here — it means the gate
// left nothing behind.
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
