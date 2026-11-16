//go:build unix

package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// unit is the child's process group. The child leads it, so its id is the child's pid. A group id is not given to
// a new process while anyone is still in the group, so signalling it never reaches a stranger.
type unit struct {
	pgid int
}

func startInUnit(cmd *exec.Cmd) (unit, error) {
	cmd.SysProcAttr = sysProcAttr()
	if err := cmd.Start(); err != nil {
		return unit{}, err
	}
	return unit{pgid: cmd.Process.Pid}, nil
}

func (u unit) signal(sig os.Signal) error {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return fmt.Errorf("%v is not a signal this platform can send", sig)
	}
	return signalGroup(u.pgid, s)
}

func (u unit) terminate() error {
	return signalGroup(u.pgid, syscall.SIGTERM)
}

func (u unit) kill() error {
	return signalGroup(u.pgid, syscall.SIGKILL)
}

// empty reports whether nobody is left in the group, the child included.
func (u unit) empty() bool {
	return errors.Is(syscall.Kill(-u.pgid, 0), syscall.ESRCH)
}

// GroupAlive reports whether anyone is still in the group pgid led, which can hold a child's descendants after the
// child itself is gone.
func GroupAlive(pgid int) bool {
	return pgid > 1 && !errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}

// KillGroup kills every process in the group pgid leads, and the process with that pid, which a child started without
// a group of its own needs. A group or process already gone is not an error.
func KillGroup(pgid int) error {
	if pgid <= 1 {
		return fmt.Errorf("refusing to kill group %d", pgid)
	}
	if err := signalGroup(pgid, syscall.SIGKILL); err != nil {
		return err
	}
	if err := syscall.Kill(pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func signalGroup(pgid int, sig syscall.Signal) error {
	err := syscall.Kill(-pgid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func exitOf(state *os.ProcessState) Exit {
	if state == nil {
		return Exit{Code: -1}
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return Exit{Code: -1, Signal: status.Signal().String(), signal: status.Signal()}
	}
	return Exit{Code: state.ExitCode()}
}
