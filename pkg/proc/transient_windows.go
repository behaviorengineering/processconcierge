package proc

import (
	"errors"

	"golang.org/x/sys/windows"
)

// transientStart reports whether Windows refused a start for a reason that passes: another program, such as an
// antivirus scanner, holding the program file, or too little memory for now.
func transientStart(err error) bool {
	for _, passing := range []windows.Errno{windows.ERROR_SHARING_VIOLATION, windows.ERROR_NOT_ENOUGH_MEMORY} {
		if errors.Is(err, passing) {
			return true
		}
	}
	return false
}
