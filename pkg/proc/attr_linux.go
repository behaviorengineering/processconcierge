package proc

import "syscall"

// The child gets a group of its own and, with parentDeath, Linux is asked to kill it when the thread that started it
// dies. Go only ends a thread when a goroutine locked to it exits, which this package never does, so in practice the
// signal comes when the parent process dies. A reaper is started without it, since it must live on to stop its child.
func sysProcAttr(parentDeath bool) *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setpgid: true}
	if parentDeath {
		attr.Pdeathsig = syscall.SIGKILL
	}
	return attr
}
