//go:build !windows

package api

import (
	"os"
	"os/exec"
	"syscall"
)

// Keep all CLI children in a private process group and kill that group both on
// deadline and on normal completion, rather than leaving a wrapper/helper alive.
func codexCLIRun(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if err == syscall.ESRCH {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	return cmd.Wait()
}
