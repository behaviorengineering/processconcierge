package pressure

import "unsafe"

// unsafePointer hands a Win32 call the address of an out parameter.
func unsafePointer(p *int32) unsafe.Pointer {
	return unsafe.Pointer(p) //nolint:gosec // G103: the Win32 call writes its answer through this pointer.
}
