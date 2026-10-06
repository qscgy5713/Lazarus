//go:build windows

package fetch

import "os/exec"

func prepareCmd(cmd *exec.Cmd) {
	// On Windows, CommandContext defaults to TerminateProcess which terminates the process.
}
