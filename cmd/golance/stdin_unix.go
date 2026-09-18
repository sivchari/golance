//go:build unix

package main

import (
	"io"
	"os"
	"syscall"
)

// pollableStdin returns a version of stdin that supports SetReadDeadline,
// converting it in place if necessary.
//
// A real editor-launched golance inherits fd 0 from its parent in blocking
// mode, which the Go runtime never registers with its I/O poller; calling
// SetReadDeadline on that *os.File is a silent no-op (returns
// os.ErrNoDeadline internally, swallowed by ClientGone below), so it can
// never unblock Serve's read loop. ClientGone's graceful teardown depends
// entirely on stdin actually honoring a deadline — without this, every
// client-death shutdown falls through to the time.AfterFunc os.Exit
// backstop instead, which never gives the indexer subprocess a chance to be
// killed via its own ctx cancellation.
func pollableStdin(stdin io.Reader) io.Reader {
	f, ok := stdin.(*os.File)
	if !ok {
		return stdin
	}
	// Fd() itself forces f into blocking mode as a side effect, so it must
	// run before SetNonblock below, not after.
	fd := f.Fd()
	if err := syscall.SetNonblock(int(fd), true); err != nil {
		return f
	}
	// NewFile observes the O_NONBLOCK flag just set above and registers fd
	// with the runtime poller, which is what makes SetReadDeadline work.
	return os.NewFile(fd, f.Name())
}
