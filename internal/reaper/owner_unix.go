//go:build unix

package reaper

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// OwnerPipe opens the owner's pipe from an inherited file descriptor. It is marked close-on-exec, so the child and
// what it starts never hold it open, and non-blocking, so Go's poller watches it and closing it ends a read.
func OwnerPipe(fd int) *os.File {
	syscall.CloseOnExec(fd)
	_ = syscall.SetNonblock(fd, true) // a descriptor that refuses this is still read, only not interruptibly
	return os.NewFile(uintptr(fd), "owner")
}

// ownerPoll is how often the reaper looks whether its owner's process is still there.
const ownerPoll = 200 * time.Millisecond

// watchPID closes the channel it returns once no process has that pid, and stops looking when done closes. It looks
// every ownerPoll, so a pid reused within that time would be missed; the owner's pipe has no such gap, and is the
// better watch where the owner can pass one.
func watchPID(pid int, done <-chan struct{}) <-chan struct{} {
	gone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(ownerPoll)
		defer ticker.Stop()
		for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
		close(gone)
	}()
	return gone
}
