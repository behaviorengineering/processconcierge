package proc

import (
	"errors"
	"os"
	"os/exec"
)

// errNoJobObjects is returned until the Windows backend puts each child in a kill-on-close Job Object.
var errNoJobObjects = errors.New("starting a child in a unit needs Job Objects, which this version does not support on Windows yet")

type unit struct{}

func startInUnit(*exec.Cmd) (unit, error) {
	return unit{}, errNoJobObjects
}

func (unit) signal(os.Signal) error { return errNoJobObjects }

func (unit) terminate() error { return errNoJobObjects }

func (unit) kill() error { return errNoJobObjects }

func (unit) empty() bool { return true }

// GroupAlive is not supported on Windows yet, so it reports no group.
func GroupAlive(int) bool {
	return false
}

// KillGroup is not supported on Windows yet.
func KillGroup(int) error {
	return errNoJobObjects
}

func exitOf(state *os.ProcessState) Exit {
	if state == nil {
		return Exit{Code: -1}
	}
	return Exit{Code: state.ExitCode()}
}
