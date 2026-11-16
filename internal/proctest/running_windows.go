package proctest

import "golang.org/x/sys/windows"

// Running reports whether the process with that pid has not yet exited.
func Running(pid int) bool {
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid)) //nolint:gosec // G115: Windows process ids are 32-bit.
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(process) }()
	event, err := windows.WaitForSingleObject(process, 0)
	return err == nil && event == uint32(windows.WAIT_TIMEOUT)
}
