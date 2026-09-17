//go:build windows

package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// memoryStatusEx mirrors the Win32 MEMORYSTATUSEX struct: golang.org/x/sys/windows
// (v0.47.0) does not wrap GlobalMemoryStatusEx itself, so physicalMemory
// below calls it directly via a LazyDLL/LazyProc, the same binding style
// x/sys/windows uses internally for the syscalls it does wrap.
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

var procGlobalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

// physicalMemory reports the machine's total physical memory in bytes, via
// the GlobalMemoryStatusEx Win32 API.
func physicalMemory() (uint64, bool) {
	var status memoryStatusEx
	status.length = uint32(unsafe.Sizeof(status))
	ret, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	if ret == 0 {
		return 0, false
	}
	return status.totalPhys, true
}
