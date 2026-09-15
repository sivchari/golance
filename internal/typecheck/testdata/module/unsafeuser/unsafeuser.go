// Package unsafeuser directly imports "unsafe", mirroring the shape
// protoc-gen-go emits (an unsafe.Pointer conversion, plus the same
// blank "for linkname" import go/types' own api.go uses).
package unsafeuser

import "unsafe"

// AsPointer converts v to an unsafe.Pointer.
func AsPointer(v *int) unsafe.Pointer {
	return unsafe.Pointer(v)
}
