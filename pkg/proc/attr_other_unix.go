//go:build unix && !linux

package proc

import "syscall"

// The child gets a group of its own. On macOS there is no parent-death signal; a reaper watching its owner stands in
// for one.
func sysProcAttr(bool) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
