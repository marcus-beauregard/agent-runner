//go:build windows

package gate

import "os/exec"

// setProcessGroup does nothing: Windows has no process group to kill, so a
// cancelled gate stops at its shell.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup reports nothing killed, since there is no group to kill.
func killProcessGroup(cmd *exec.Cmd) (bool, error) { return false, nil }
