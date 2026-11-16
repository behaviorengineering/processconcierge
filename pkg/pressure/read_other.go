//go:build !darwin && !linux && !windows

package pressure

import (
	"context"
	"strconv"

	"github.com/shirou/gopsutil/v4/mem"
)

func read(ctx context.Context) (Reading, error) {
	vm, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return Reading{}, err
	}
	available := 100 - vm.UsedPercent
	return Reading{
		Level:  LevelFromAvailable(available),
		Source: "available memory",
		Detail: "available=" + strconv.FormatFloat(available, 'f', 1, 64) + "%",
	}, nil
}
