package pressure

import (
	"context"
	"sync"

	"golang.org/x/sys/windows"
)

var (
	kernel32                         = windows.NewLazySystemDLL("kernel32.dll")
	procCreateMemoryResourceNotifier = kernel32.NewProc("CreateMemoryResourceNotification")
	procQueryMemoryResourceNotifier  = kernel32.NewProc("QueryMemoryResourceNotification")
	lowMemory                        windows.Handle
	lowMemoryOnce                    sync.Once
)

// lowMemoryResourceNotification is LowMemoryResourceNotification in MEMORY_RESOURCE_NOTIFICATION_TYPE.
const lowMemoryResourceNotification = 0

// lowMemorySignalled reports whether Windows's own low-memory notification is set, and false when it cannot be read.
func lowMemorySignalled() bool {
	lowMemoryOnce.Do(func() {
		handle, _, _ := procCreateMemoryResourceNotifier.Call(lowMemoryResourceNotification)
		lowMemory = windows.Handle(handle)
	})
	if lowMemory == 0 {
		return false
	}
	var state int32
	ok, _, _ := procQueryMemoryResourceNotifier.Call(uintptr(lowMemory), uintptr(unsafePointer(&state)))
	return ok != 0 && state != 0
}

func read(ctx context.Context) (Reading, error) {
	r, err := fromAvailable(ctx)
	if err != nil {
		return Reading{}, err
	}
	if lowMemorySignalled() {
		r.Level = Critical
		r.Source = "low-memory notification"
	}
	return r, nil
}
