//go:build !darwin

package pressure

import (
	"context"
	"strconv"

	"github.com/shirou/gopsutil/v4/mem"
)

// fromAvailable judges the share of memory still available: Linux's fallback, and the reading on Windows and other
// systems.
func fromAvailable(ctx context.Context) (Reading, error) {
	vm, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return Reading{}, err
	}
	available := 100 - vm.UsedPercent
	return Reading{
		Level:  levelFromAvailable(available),
		Source: "available memory",
		Detail: "available=" + strconv.FormatFloat(available, 'f', 1, 64) + "%",
	}, nil
}
