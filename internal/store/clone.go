package store

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ClonePath copies the bbolt database file at src to dst without ever
// opening either path as a bbolt database itself, preferring an OS-level
// copy-on-write clone where available (see platformClone) and falling back
// to a streaming byte copy otherwise.
//
// Why: src may be a shared index database another live session currently
// holds open for writing, so a clone taken mid-write can capture a torn,
// transactionally-inconsistent snapshot — but a later store.Open(dst)
// already discards and recreates exactly that case via its corrupt-file
// self-heal (see Open's own doc), leaving an empty database no worse than
// never having built one at all. Never opening src itself also means this
// can never contend for, or otherwise disturb, whatever exclusive lock a
// live writer holds on it.
func ClonePath(src, dst string) error {
	if err := platformClone(src, dst); err == nil {
		return nil
	}
	return streamCopy(src, dst)
}

// streamCopy is ClonePath's fallback for a platform (or a platform-specific
// failure) with no copy-on-write clone available: an ordinary whole-file
// read/write, slower than platformClone but identical in outcome.
func streamCopy(src, dst string) error {
	in, err := os.Open(filepath.Clean(src))
	if err != nil {
		return fmt.Errorf("store: clone %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(filepath.Clean(dst), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("store: clone %s to %s: %w", src, dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("store: clone %s to %s: %w", src, dst, err)
	}
	return out.Close()
}
