package pressure

import (
	"context"
	"os"
	"strconv"

	"github.com/shirou/gopsutil/v4/mem"
)

func read(ctx context.Context) (Reading, error) {
	if data, err := os.ReadFile("/proc/pressure/memory"); err == nil {
		if level, detail, ok := LevelFromPSI(string(data)); ok {
			return Reading{Level: level, Source: "/proc/pressure/memory", Detail: detail}, nil
		}
	}
	// Kernels before 4.20, and some containers, have no pressure stall information.
	return fromAvailable(ctx)
}

func fromAvailable(ctx context.Context) (Reading, error) {
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
