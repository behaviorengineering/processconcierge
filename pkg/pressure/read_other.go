//go:build !darwin && !linux && !windows

package pressure

import "context"

func read(ctx context.Context) (Reading, error) {
	return fromAvailable(ctx)
}
