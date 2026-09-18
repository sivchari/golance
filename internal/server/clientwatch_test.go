package server

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/rpc"
)

// errIntentionalTestGraphLoadFailure stands in for a real graph.Load
// failure: these tests only care about processId wiring, not about a
// successful workspace load, so failing fast avoids running a real `go
// list` inside a synctest bubble.
var errIntentionalTestGraphLoadFailure = errors.New("intentional test graph load failure")

// TestWatchClientProcess_ClientGoneWhenProcessDies verifies watchClientProcess
// polls processAlive on clientWatchInterval and calls ClientGone exactly once
// after it starts reporting the client dead — never while it still reports
// alive.
func TestWatchClientProcess_ClientGoneWhenProcessDies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
		s := New(rpcServer, Options{Logger: newTestLogger(t)})

		var alive atomic.Bool
		alive.Store(true)
		orig := processAlive
		processAlive = func(int) bool { return alive.Load() }
		t.Cleanup(func() { processAlive = orig })

		gone := make(chan struct{})
		s.opts.ClientGone = func() { close(gone) }

		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go s.watchClientProcess(ctx, 12345)

		time.Sleep(clientWatchInterval)
		synctest.Wait()
		select {
		case <-gone:
			t.Fatal("ClientGone called while processAlive still reports alive")
		default:
		}

		alive.Store(false)
		time.Sleep(clientWatchInterval)
		synctest.Wait()
		select {
		case <-gone:
		default:
			t.Fatal("ClientGone not called after processAlive started reporting the process dead")
		}
	})
}

// TestWatchClientProcess_StopsOnContextCancel verifies watchClientProcess
// returns once its ctx is canceled (the session ending normally), without
// ever calling ClientGone — a goroutine-leak proof mirroring
// TestGo_TrackedByServeShutdownDrain in internal/rpc.
func TestWatchClientProcess_StopsOnContextCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
		s := New(rpcServer, Options{Logger: newTestLogger(t)})

		orig := processAlive
		processAlive = func(int) bool { return true }
		t.Cleanup(func() { processAlive = orig })

		called := make(chan struct{})
		s.opts.ClientGone = func() { close(called) }

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			s.watchClientProcess(ctx, 1)
			close(done)
		}()

		cancel()
		synctest.Wait()

		select {
		case <-done:
		default:
			t.Fatal("watchClientProcess did not return after ctx was canceled")
		}
		select {
		case <-called:
			t.Fatal("ClientGone called on ctx cancellation, not a process death")
		default:
		}
	})
}

// TestHandleInitialize_NoProcessIDNoWatch verifies handleInitialize starts no
// watchClientProcess goroutine when initialize's processId is absent: even
// with processAlive faked to immediately report the (nonexistent) client
// dead, ClientGone is never called.
func TestHandleInitialize_NoProcessIDNoWatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		orig := graphLoad
		graphLoad = func(graph.Options, ...string) (*graph.Snapshot, error) {
			return nil, errIntentionalTestGraphLoadFailure
		}
		t.Cleanup(func() { graphLoad = orig })

		origAlive := processAlive
		processAlive = func(int) bool { return false }
		t.Cleanup(func() { processAlive = origAlive })

		rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
		s := New(rpcServer, Options{Logger: newTestLogger(t)})
		called := make(chan struct{})
		s.opts.ClientGone = func() { close(called) }

		root := t.TempDir()
		params := mustMarshal(t, &protocol.InitializeParams{
			WorkspaceFoldersInitializeParams: protocol.WorkspaceFoldersInitializeParams{
				WorkspaceFolders: protocol.NewNullable([]protocol.WorkspaceFolder{{URI: uri.File(root), Name: "root"}}),
			},
		})
		if _, err := s.handleInitialize(context.Background(), params); err != nil {
			t.Fatalf("handleInitialize: %v", err)
		}

		time.Sleep(2 * clientWatchInterval)
		synctest.Wait()

		select {
		case <-called:
			t.Fatal("ClientGone called even though initialize had no processId")
		default:
		}
	})
}

// TestHandleInitialize_StartsWatcherForProcessID verifies handleInitialize
// wires processId into a watchClientProcess goroutine (rather than only unit
// testing watchClientProcess in isolation): with processAlive faked to
// report the client dead immediately, ClientGone fires within one
// clientWatchInterval tick of "initialize" returning.
func TestHandleInitialize_StartsWatcherForProcessID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		orig := graphLoad
		graphLoad = func(graph.Options, ...string) (*graph.Snapshot, error) {
			return nil, errIntentionalTestGraphLoadFailure
		}
		t.Cleanup(func() { graphLoad = orig })

		origAlive := processAlive
		processAlive = func(int) bool { return false }
		t.Cleanup(func() { processAlive = origAlive })

		rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
		s := New(rpcServer, Options{Logger: newTestLogger(t)})
		called := make(chan struct{})
		s.opts.ClientGone = func() { close(called) }

		root := t.TempDir()
		pid := int32(99999)
		params := mustMarshal(t, &protocol.InitializeParams{
			WorkspaceFoldersInitializeParams: protocol.WorkspaceFoldersInitializeParams{
				WorkspaceFolders: protocol.NewNullable([]protocol.WorkspaceFolder{{URI: uri.File(root), Name: "root"}}),
			},
			ProcessID: &pid,
		})
		if _, err := s.handleInitialize(context.Background(), params); err != nil {
			t.Fatalf("handleInitialize: %v", err)
		}

		time.Sleep(clientWatchInterval)
		synctest.Wait()

		select {
		case <-called:
		default:
			t.Fatal("ClientGone was not called; handleInitialize did not start watchClientProcess for initialize's processId")
		}
	})
}

// TestProcessAlive_RealProcess verifies processAlive against real OS
// processes rather than only exercising the fake: the current process's own
// pid must report alive, and a child process that has already been killed
// and reaped must report dead.
func TestProcessAlive_RealProcess(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("processAlive(os.Getpid()) = false, want true")
	}

	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill sleep: %v", err)
	}
	_ = cmd.Wait()

	if processAlive(pid) {
		t.Fatalf("processAlive(%d) = true after kill+Wait, want false", pid)
	}
}
