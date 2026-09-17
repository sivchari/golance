//go:build darwin

package main

import "golang.org/x/sys/unix"

// physicalMemory reports the machine's total physical memory in bytes, via
// the hw.memsize sysctl.
func physicalMemory() (uint64, bool) {
	v, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, false
	}
	return v, true
}
