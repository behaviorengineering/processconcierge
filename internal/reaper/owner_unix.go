//go:build unix

package reaper

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// OwnerPipe opens the owner's pipe from an inherited file descriptor, marked close-on-exec so the child and what it
// starts never hold it open.
func OwnerPipe(fd int) *os.File {
	syscall.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), "owner")
}

// ownerPoll is how often the reaper looks whether its owner's process is still there.
const ownerPoll = 200 * time.Millisecond

// watchPID closes the channel it returns once no process has that pid. It looks every ownerPoll, so a pid reused
// within that time would be missed; the owner's pipe has no such gap, and is the better watch where the owner can
// pass one.
func watchPID(pid int) <-chan struct{} {
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			time.Sleep(ownerPoll)
		}
	}()
	return gone
}
