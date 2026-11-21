//go:build windows

package tools

import (
	"os"
	"os/exec"
)

// setProcessGroup is a no-op on Windows, which has no process groups in the
// Unix sense. CommandContext's default Cancel still kills the direct process
// when the deadline fires; children it started are not reached, and the
// container sandbox is what must contain them (PRD §6.2).
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup is a no-op on Windows. See setProcessGroup.
func killProcessGroup(cmd *exec.Cmd) error { return nil }

// exitStatus reports the exit code; Windows records no signal names.
func exitStatus(state *os.ProcessState) (int, string) {
	if state == nil {
		return -1, ""
	}
	return state.ExitCode(), ""
}
