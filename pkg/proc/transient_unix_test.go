//go:build unix

package proc

import (
	"fmt"
	"os"
	"syscall"
	"testing"
)

func TestOnlyStartsRefusedForAPassingReasonAreRetried(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EAGAIN, syscall.EMFILE, syscall.ENFILE, syscall.ENOMEM, syscall.ETXTBSY} {
		if !retryable(&os.PathError{Op: "fork/exec", Path: "/bin/claude", Err: errno}) {
			t.Errorf("%v is not retried", errno)
		}
	}
	for _, errno := range []syscall.Errno{syscall.ENOENT, syscall.EACCES, syscall.ENOEXEC} {
		if retryable(fmt.Errorf("start: %w", errno)) {
			t.Errorf("%v is retried, though it never passes by itself", errno)
		}
	}
	if retryable(fmt.Errorf("%w: %w", errAfterCreate, syscall.EAGAIN)) {
		t.Error("a passing error after the child existed is retried")
	}
}
