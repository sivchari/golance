//go:build windows

package main

import "io"

// pollableStdin returns stdin unchanged on Windows: syscall.SetNonblock
// takes a syscall.Handle there and an inherited console or pipe handle
// cannot be made poller-registered this way, so ClientGone's read-deadline
// unblock is unavailable and its timed os.Exit backstop is the exit path
// when the client process dies.
func pollableStdin(stdin io.Reader) io.Reader {
	return stdin
}
