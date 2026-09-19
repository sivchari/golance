//go:build darwin

package store

import "golang.org/x/sys/unix"

// platformClone clones src to dst via APFS's clonefile(2) syscall: an
// instant, copy-on-write duplication that never reads src's actual bytes
// (see ClonePath's own doc for why that matters against a possibly-live
// writer). Its error (e.g. dst already exists, or src/dst are on
// filesystems that do not support cloning) is returned as-is so ClonePath
// can fall back to streamCopy instead of surfacing it as its own failure.
func platformClone(src, dst string) error {
	return unix.Clonefile(src, dst, 0)
}
