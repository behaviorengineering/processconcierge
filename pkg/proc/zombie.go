package proc

import (
	"slices"

	"github.com/shirou/gopsutil/v4/process"
)

// zombie reports whether the process with that pid is already dead and only waiting to be collected, which a process
// that is gone altogether counts as too. A process whose state cannot be read is taken as not a zombie.
func zombie(pid int) bool {
	if pid <= 0 || pid > 1<<31-1 {
		return true
	}
	p, err := process.NewProcess(int32(pid)) //nolint:gosec // G115: range checked above.
	if err != nil {
		return true
	}
	status, err := p.Status()
	if err != nil {
		return false
	}
	return slices.Contains(status, process.Zombie)
}
