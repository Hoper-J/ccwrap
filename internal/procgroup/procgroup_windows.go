package procgroup

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// leaderEnv marks the copy of ccwrap that Respawn starts.
const leaderEnv = "CCWRAP_PROCESS_GROUP_LEADER"

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")

	leader bool
)

// Lead reports whether this process is a copy that Respawn started. Such a
// copy leads its group: Lead turns Ctrl+C back on, which a new group starts
// with off and Claude Code inherits, and drops the marker so that Claude
// Code does not inherit that too.
func Lead() bool {
	if os.Getenv(leaderEnv) != "1" {
		return false
	}
	os.Unsetenv(leaderEnv)
	procSetConsoleCtrlHandler.Call(0, 0)
	leader = true
	return true
}

// Leader reports whether Lead made this process its group's leader.
func Leader() bool { return leader }

// Interrupt sends Ctrl+C to this process's group: ccwrap itself, Claude
// Code, and what Claude Code started.
func Interrupt() error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_C_EVENT, uint32(os.Getpid()))
}

// Respawn runs this program again with args, as the leader of a new process
// group on this console, and returns its exit code once it exits. bind ties
// the copy's lifetime to this process. Meanwhile Ctrl+C is the copy's to
// handle; this process ignores it. ok is false, and the caller carries on
// by itself, without a console or when the copy cannot start.
func Respawn(args []string, bind func(*os.Process)) (code int, ok bool) {
	var pid uint32
	if n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pid)), 1); n == 0 {
		return 0, false
	}
	exe, err := os.Executable()
	if err != nil {
		return 0, false
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), leaderEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	if err := cmd.Start(); err != nil {
		return 0, false
	}
	bind(cmd.Process)
	_ = cmd.Wait()
	return cmd.ProcessState.ExitCode(), true
}
