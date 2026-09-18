//go:build unix

package main

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

// pinnedStdinWrappers holds every pre-pollableStdin *os.File a test handed
// to pollableStdin, for the life of the test binary. pollableStdin
// deliberately makes a second *os.File own the same fd (in production both
// live until process exit, so nothing ever double-closes), but in a test the
// unclosed wrapper becomes garbage once its test returns, and its finalizer
// would then close an fd NUMBER that a later test's subprocess pipe may have
// reused — observed in CI as go/packages' `go env` reader blocking forever
// in TestLoadGraph_ReusesCacheWhenNotStale after this test ran. Pinning the
// wrapper keeps that finalizer from ever running; the fd itself is still
// closed exactly once, through the pollableStdin result's own Cleanup.
var pinnedStdinWrappers []*os.File

// TestPollableStdin_MakesInheritedBlockingPipeDeadlineCapable is a
// regression test for a real e2e finding: an inherited fd 0 (a child
// process's real stdin, as opposed to an os.Pipe() created in-process) is in
// blocking mode and so is never registered with the runtime poller, making
// File.SetReadDeadline silently ineffective on it (os.ErrNoDeadline).
// pollableStdin must produce a *os.File that DOES support deadlines from
// one that does not.
//
// The pipe is created with raw syscall.Pipe, not os.Pipe: its fds start out
// blocking and were never registered with the runtime poller, exactly the
// state an inherited fd 0 is found in. Re-wrapping an os.Pipe fd instead
// would leave a stale epoll registration behind on Linux, where a second
// registration of the same fd fails (EEXIST) and silently forces
// pollableStdin's result back into blocking mode — a test-only artifact a
// real inherited stdin can never hit.
func TestPollableStdin_MakesInheritedBlockingPipeDeadlineCapable(t *testing.T) {
	var p [2]int
	if err := syscall.Pipe(p[:]); err != nil {
		t.Fatalf("syscall.Pipe: %v", err)
	}
	// Raw syscall.Pipe fds lack O_CLOEXEC (unlike os.Pipe's), and this test
	// binary later execs `go list` subprocesses that must not inherit them.
	syscall.CloseOnExec(p[0])
	syscall.CloseOnExec(p[1])
	t.Cleanup(func() { _ = syscall.Close(p[1]) })

	blocking := os.NewFile(uintptr(p[0]), "inherited-stdin")
	pinnedStdinWrappers = append(pinnedStdinWrappers, blocking)

	if err := blocking.SetReadDeadline(time.Now()); !errors.Is(err, os.ErrNoDeadline) {
		t.Fatalf("SetReadDeadline on the simulated inherited blocking fd = %v, want os.ErrNoDeadline (test setup does not reproduce the bug)", err)
	}

	pollable := pollableStdin(blocking)
	pf, ok := pollable.(*os.File)
	if !ok {
		t.Fatalf("pollableStdin() returned %T, want *os.File", pollable)
	}
	// The one and only close of the shared fd; blocking stays pinned above.
	t.Cleanup(func() { _ = pf.Close() })

	readErr := make(chan error, 1)
	go func() {
		_, err := pf.Read(make([]byte, 1))
		readErr <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the Read above actually block first
	if err := pf.SetReadDeadline(time.Now()); err != nil {
		t.Fatalf("SetReadDeadline() on pollableStdin's result = %v, want nil", err)
	}

	select {
	case err := <-readErr:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Read() error = %v, want os.ErrDeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read() did not unblock after SetReadDeadline; pollableStdin did not make the fd poller-registered")
	}
}
