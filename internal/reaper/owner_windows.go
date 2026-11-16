package reaper

import "os"

// OwnerPipe opens the owner's pipe from an inherited handle.
func OwnerPipe(fd int) *os.File {
	return os.NewFile(uintptr(fd), "owner")
}
