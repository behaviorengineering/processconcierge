package proc

import "syscall"

// The child gets a group of its own, and Linux is asked to kill it when the thread that started it dies.
// Go only ends a thread when a goroutine locked to it exits, which this package never does, so in practice the signal
// comes when the parent process dies.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
