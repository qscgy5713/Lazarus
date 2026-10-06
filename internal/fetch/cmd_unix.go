//go:build !windows

package fetch

import (
	"os/exec"
	"syscall"
)

func prepareCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Kill the entire process group so child processes (like sleep spawned by sh)
		// are killed immediately when context times out, closing pipes without blocking.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
