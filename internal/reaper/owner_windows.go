package reaper

import (
	"os"

	"golang.org/x/sys/windows"
)

// OwnerPipe opens the owner's pipe from an inherited handle. The command refuses --owner-fd on Windows, where Node
// cannot pass one; this exists for callers that hand a pipe's handle through some other way.
func OwnerPipe(fd int) *os.File {
	return os.NewFile(uintptr(fd), "owner")
}

// ownerWaitMS is how long each wait on the owner's process lasts before the watch looks whether it should end.
const ownerWaitMS = 200

// watchPID closes the channel it returns once the process with that pid exits, and stops waiting when done closes.
// It waits on a handle to the process, which keeps the process's identity, so a reused pid cannot fool it. A process
// that cannot be opened counts as gone.
func watchPID(pid int, done <-chan struct{}) <-chan struct{} {
	gone := make(chan struct{})
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid)) //nolint:gosec // G115: Windows process ids are 32-bit.
	if err != nil {
		close(gone)
		return gone
	}
	go func() {
		defer func() { _ = windows.CloseHandle(process) }()
		for {
			event, err := windows.WaitForSingleObject(process, ownerWaitMS)
			if err != nil || event != uint32(windows.WAIT_TIMEOUT) {
				close(gone)
				return
			}
			select {
			case <-done:
				return
			default:
			}
		}
	}()
	return gone
}
