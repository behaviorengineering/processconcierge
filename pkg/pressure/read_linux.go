package pressure

import (
	"context"
	"os"
)

func read(ctx context.Context) (Reading, error) {
	if data, err := os.ReadFile("/proc/pressure/memory"); err == nil {
		if level, detail, ok := levelFromPSI(string(data)); ok {
			return Reading{Level: level, Source: "/proc/pressure/memory", Detail: detail}, nil
		}
	}
	// Kernels before 4.20, and some containers, have no pressure stall information.
	return fromAvailable(ctx)
}
