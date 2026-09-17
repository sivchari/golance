//go:build linux

package main

import "golang.org/x/sys/unix"

// physicalMemory reports the machine's total physical memory in bytes, via
// the sysinfo(2) syscall.
func physicalMemory() (uint64, bool) {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return 0, false
	}
	return uint64(info.Totalram) * uint64(info.Unit), true
}
