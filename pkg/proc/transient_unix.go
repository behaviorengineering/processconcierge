//go:build unix

package proc

import (
	"errors"
	"syscall"
)

// transientStart reports whether the system refused a start for a reason that passes: too many processes or open
// files, too little memory for now, or the program file busy because it is being replaced.
func transientStart(err error) bool {
	for _, passing := range []syscall.Errno{syscall.EAGAIN, syscall.EMFILE, syscall.ENFILE, syscall.ENOMEM, syscall.ETXTBSY} {
		if errors.Is(err, passing) {
			return true
		}
	}
	return false
}
