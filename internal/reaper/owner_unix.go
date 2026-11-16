//go:build unix

package reaper

import (
	"os"
	"syscall"
)

// OwnerPipe opens the owner's pipe from an inherited file descriptor, marked close-on-exec so the child and what it
// starts never hold it open.
func OwnerPipe(fd int) *os.File {
	syscall.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), "owner")
}
