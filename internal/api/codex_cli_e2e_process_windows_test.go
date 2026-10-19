//go:build windows

package api

import (
	"fmt"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A private kill-on-close Job Object covers the native CLI and any startup
// helper it creates. Never invoke taskkill/cmd.exe against an ambient process.
func codexCLIRun(cmd *exec.Cmd) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create private CLI job: %w", err)
	}
	defer windows.CloseHandle(job)
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return fmt.Errorf("configure CLI job: %w", err)
	}
	cmd.Cancel = func() error {
		if err := windows.TerminateJobObject(job, 1); err != nil {
			if killErr := cmd.Process.Kill(); killErr != nil {
				return killErr
			}
		}
		return nil
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("open native CLI process for job: %w", err)
	}
	err = windows.AssignProcessToJobObject(job, process)
	_ = windows.CloseHandle(process)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("assign native CLI process to job: %w", err)
	}
	err = cmd.Wait()
	// Closing the private job on all paths also terminates lingering children.
	return err
}
