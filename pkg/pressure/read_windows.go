package pressure

import (
	"context"
	"strconv"
	"sync"

	"github.com/shirou/gopsutil/v4/mem"
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
	vm, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return Reading{}, err
	}
	available := 100 - vm.UsedPercent
	detail := "available=" + strconv.FormatFloat(available, 'f', 1, 64) + "%"
	if lowMemorySignalled() {
		return Reading{Level: Critical, Source: "low-memory notification", Detail: detail}, nil
	}
	return Reading{Level: LevelFromAvailable(available), Source: "available memory", Detail: detail}, nil
}
