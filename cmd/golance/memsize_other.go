//go:build !darwin && !linux && !windows

package main

// physicalMemory always reports unknown: no OS-specific implementation
// exists for this GOOS.
func physicalMemory() (uint64, bool) {
	return 0, false
}
