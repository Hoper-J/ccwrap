package main

import (
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// childSysProcAttr adds nothing on Windows; bindChildLifetime ties Claude
// Code to ccwrap there.
func childSysProcAttr() *syscall.SysProcAttr { return nil }

// childJob holds Claude Code's job object for as long as ccwrap runs. It is
// never closed on purpose: Windows closes it when ccwrap exits, however it
// exits, and then ends every process in the job.
var childJob windows.Handle

// bindChildLifetime puts the child in a job that Windows kills when ccwrap's
// handle to it closes, so a killed ccwrap does not leave Claude Code running
// against a dead proxy. Like Pdeathsig, it ends Claude Code alone: what
// Claude Code starts is not in the job (silent breakaway), so its own
// helpers still finish after it exits, the npm run that updates it among
// them, which leaves no claude.exe if cut short.
func bindChildLifetime(p *os.Process) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return err
	}
	// The child cannot have exited and had its id reused: exec.Cmd holds a
	// handle to it until Wait.
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return err
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return err
	}
	childJob = job
	return nil
}
