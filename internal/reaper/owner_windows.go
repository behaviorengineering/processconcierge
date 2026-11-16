package reaper

import (
	"os"

	"golang.org/x/sys/windows"
)

// OwnerPipe opens the owner's pipe from an inherited handle.
func OwnerPipe(fd int) *os.File {
	return os.NewFile(uintptr(fd), "owner")
}

// watchPID closes the channel it returns once the process with that pid exits. It waits on a handle to the process,
// which keeps the process's identity, so a reused pid cannot fool it. A process that cannot be opened counts as gone.
func watchPID(pid int) <-chan struct{} {
	gone := make(chan struct{})
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid)) //nolint:gosec // G115: Windows process ids are 32-bit.
	if err != nil {
		close(gone)
		return gone
	}
	go func() {
		defer close(gone)
		defer func() { _ = windows.CloseHandle(process) }()
		_, _ = windows.WaitForSingleObject(process, windows.INFINITE)
	}()
	return gone
}
