package pressure

import (
	"context"
	"strconv"

	"golang.org/x/sys/unix"
)

func read(context.Context) (Reading, error) {
	const name = "kern.memorystatus_vm_pressure_level"
	value, err := unix.SysctlUint32(name)
	if err != nil {
		return Reading{}, err
	}
	return Reading{Level: LevelFromDarwin(value), Source: name, Detail: name + "=" + strconv.FormatUint(uint64(value), 10)}, nil
}
