//go:build linux

package main

import "syscall"

// childSysProcAttr asks the kernel to send the child SIGTERM if THIS
// process dies (Pdeathsig). It is the Linux-only guarantee for the hard
// failure modes Wait() cannot reach from inside a dead process: a SIGKILL
// / OOM-kill of ccwrap, or an unrecovered panic in a bare goroutine that
// terminates the runtime without running defers. Without it, Claude is
// reparented to init and keeps running against a now-dead proxy, failing
// every request.
//
// Caveat: Pdeathsig is keyed to the parent THREAD, and Go's scheduler can
// migrate goroutines across OS threads. exec.Cmd.Start does the fork with
// the thread locked, so the common whole-process-death case delivers the
// signal correctly; the residual risk is a premature signal if that exact
// thread exits while the process lives — rare, and strictly preferable to
// an orphaned Claude talking to a dead proxy.
func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
