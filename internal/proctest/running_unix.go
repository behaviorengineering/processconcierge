//go:build unix

package proctest

import (
	"errors"
	"syscall"
)

// Running reports whether a process with that pid exists. A zombie, killed but not yet collected, still counts until
// its parent or launchd or init collects it.
func Running(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
