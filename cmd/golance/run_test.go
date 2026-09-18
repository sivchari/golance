package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sivchari/golance/internal/graph"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-version"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(-version) code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "golance") {
		t.Fatalf("run(-version) stdout = %q, want it to mention golance", stdout.String())
	}
}

func TestRunUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-does-not-exist"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("run(-does-not-exist) code = %d, want 2", code)
	}
}

func TestRunUnexpectedArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"bogus-positional-arg"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("run(bogus-positional-arg) code = %d, want 2", code)
	}
}

func TestRunServesUntilEOF(t *testing.T) {
	// No "initialize" sent: an empty client stream should make Serve
	// return cleanly (EOF), and run should report exit code 0.
	var stdout, stderr bytes.Buffer
	code := run(nil, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
}

// deadlineExceededReader's Read always fails with os.ErrDeadlineExceeded,
// standing in for a real *os.File past its read deadline: since it is not a
// *os.File, pollableStdin passes it through unchanged, letting
// TestRun_DeadlineExceededExitsZero isolate run's own serveErr handling from
// pollableStdin's fd rewrapping (covered separately by
// TestPollableStdin_MakesInheritedBlockingPipeDeadlineCapable).
type deadlineExceededReader struct{}

func (deadlineExceededReader) Read([]byte) (int, error) {
	return 0, os.ErrDeadlineExceeded
}

// TestRun_DeadlineExceededExitsZero verifies run treats stdin's own
// SetReadDeadline-induced os.ErrDeadlineExceeded as a clean, intentional
// disconnect (exit code 0) rather than a real I/O error: this is exactly
// what a ClientGone callback triggers on stdin to unblock Serve's read loop
// when the LSP client process itself has died (see server.Options.ClientGone
// and internal/rpc.Server.Serve's read loop).
func TestRun_DeadlineExceededExitsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, deadlineExceededReader{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
}

// TestPollableStdin_MakesInheritedBlockingPipeDeadlineCapable is a
// regression test for a real e2e finding: an inherited fd 0 (a child
// process's real stdin, as opposed to an os.Pipe() created in-process) is in
// blocking mode and so is never registered with the runtime poller, making
// File.SetReadDeadline silently ineffective on it (os.ErrNoDeadline).
// pollableStdin must produce a *os.File that DOES support deadlines from
// one that does not.
//
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

func TestRunIndexerRequiresEnv(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runIndexer(&stdout, &stderr)
	if code != 1 {
		t.Fatalf("runIndexer() with no env set: code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "GOLANCE_ROOT") {
		t.Fatalf("runIndexer() stderr = %q, want it to mention GOLANCE_ROOT", stderr.String())
	}
}

// TestApplyDefaultMemLimit_SetsLimitWhenUnset verifies that a GOMEMLIMIT-less
// environment gets defaultIndexerMemLimit — the case a production launch
// with neither --mem-limit nor GOLANCE_MEM_LIMIT configured hits, which
// otherwise leaves the indexer subprocess's heap unbounded (see
// defaultIndexerMemLimit's own doc for the measured peak-RSS cost of that).
func TestApplyDefaultMemLimit_SetsLimitWhenUnset(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "")
	orig := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(orig) })

	applyDefaultMemLimit(defaultIndexerMemLimit)

	if got := debug.SetMemoryLimit(-1); got != defaultIndexerMemLimit {
		t.Fatalf("SetMemoryLimit(-1) = %d, want %d", got, defaultIndexerMemLimit)
	}
}

// TestApplyDefaultMemLimit_ServerLimit verifies the SERVER path's own call
// (run, not runIndexer) applies defaultServerMemLimit — the backstop
// documented on that constant — via the identical, generalized
// applyDefaultMemLimit helper.
func TestApplyDefaultMemLimit_ServerLimit(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "")
	orig := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(orig) })

	applyDefaultMemLimit(defaultServerMemLimit)

	if got := debug.SetMemoryLimit(-1); got != defaultServerMemLimit {
		t.Fatalf("SetMemoryLimit(-1) = %d, want %d", got, defaultServerMemLimit)
	}
}

// TestApplyDefaultMemLimit_LeavesExistingLimitAlone verifies that a
// GOMEMLIMIT already present in the environment (set by internal/server
// forwarding --mem-limit/GOLANCE_MEM_LIMIT, or by a caller directly) is left
// untouched: applyDefaultMemLimit must never override a limit the runtime
// already applied from GOMEMLIMIT at process start.
func TestApplyDefaultMemLimit_LeavesExistingLimitAlone(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "8GiB")
	orig := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(orig) })

	const sentinel = 123456789
	debug.SetMemoryLimit(sentinel)

	applyDefaultMemLimit(defaultIndexerMemLimit)

	if got := debug.SetMemoryLimit(-1); got != sentinel {
		t.Fatalf("SetMemoryLimit(-1) = %d, want sentinel %d left unchanged", got, sentinel)
	}
}

// TestDeriveMemLimit verifies deriveMemLimit's floor and fraction behavior
// for both the indexer's and the server's fraction/floor pair, including the
// exact boundary where fraction*total equals floor.
func TestDeriveMemLimit(t *testing.T) {
	const GiB = int64(1 << 30)

	tests := []struct {
		name     string
		total    uint64
		ok       bool
		fraction float64
		floor    int64
		want     int64
	}{
		{
			name:     "unknown total falls back to floor",
			total:    48 << 30,
			ok:       false,
			fraction: indexerMemFraction,
			floor:    defaultIndexerMemLimit,
			want:     defaultIndexerMemLimit,
		},
		{
			name:     "16GB machine indexer stays at floor",
			total:    16 << 30,
			ok:       true,
			fraction: indexerMemFraction,
			floor:    defaultIndexerMemLimit,
			want:     4 * GiB,
		},
		{
			name:     "16GB machine server stays at floor",
			total:    16 << 30,
			ok:       true,
			fraction: serverMemFraction,
			floor:    defaultServerMemLimit,
			want:     8 * GiB,
		},
		{
			name:     "48GB machine indexer scales above floor",
			total:    48 << 30,
			ok:       true,
			fraction: indexerMemFraction,
			floor:    defaultIndexerMemLimit,
			want:     12 * GiB,
		},
		{
			name:     "48GB machine server scales above floor",
			total:    48 << 30,
			ok:       true,
			fraction: serverMemFraction,
			floor:    defaultServerMemLimit,
			want:     24 * GiB,
		},
		{
			name:     "8GB machine indexer undercuts floor",
			total:    8 << 30,
			ok:       true,
			fraction: indexerMemFraction,
			floor:    defaultIndexerMemLimit,
			want:     4 * GiB,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveMemLimit(tt.total, tt.ok, tt.fraction, tt.floor); got != tt.want {
				t.Errorf("deriveMemLimit(%d, %v, %v, %d) = %d, want %d", tt.total, tt.ok, tt.fraction, tt.floor, got, tt.want)
			}
		})
	}
}

// TestPhysicalMemory_Host verifies physicalMemory reports a plausible total
// on platforms with an OS-specific implementation, skipping on GOOS values
// that fall back to (0, false) (see memsize_other.go).
func TestPhysicalMemory_Host(t *testing.T) {
	total, ok := physicalMemory()
	if !ok {
		t.Skip("physicalMemory: not implemented on this GOOS")
	}
	const GiB = uint64(1 << 30)
	if total < GiB {
		t.Fatalf("physicalMemory() = %d, want > %d (1GiB)", total, GiB)
	}
}

// writeTinyModule writes a minimal single-package module to a fresh temp
// directory and returns its path.
func writeTinyModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/tiny\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tiny.go"), []byte("package tiny\n\n// V returns 1.\nfunc V() int { return 1 }\n"), 0o600); err != nil {
		t.Fatalf("write tiny.go: %v", err)
	}
	return dir
}

// TestLoadGraph_ReusesCacheWhenNotStale verifies that a second loadGraph
// call for the same root reuses the on-disk graph cache (graph.LoadCache)
// instead of re-running `go list`, and that touching go.mod (making the
// cache stale per graph.Stale) forces a fresh load again.
func TestLoadGraph_ReusesCacheWhenNotStale(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := writeTinyModule(t)
	opts := graph.Options{Dir: dir}
	patterns := []string{"./..."}
	var stderr bytes.Buffer

	snap1, fromCache1, err := loadGraph(opts, patterns, &stderr)
	if err != nil {
		t.Fatalf("loadGraph (first): %v", err)
	}
	if fromCache1 {
		t.Error("loadGraph (first) fromCache = true, want false (nothing cached yet)")
	}
	if len(snap1.Packages) == 0 {
		t.Fatal("loadGraph (first) returned an empty snapshot")
	}

	_, fromCache2, err := loadGraph(opts, patterns, &stderr)
	if err != nil {
		t.Fatalf("loadGraph (second): %v", err)
	}
	if !fromCache2 {
		t.Error("loadGraph (second) fromCache = false, want true (cache should be reused)")
	}

	// Touching go.mod must invalidate the cache (graph.Stale compares
	// mtimes, so the new mtime must be strictly later).
	goMod := filepath.Join(dir, "go.mod")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(goMod, future, future); err != nil {
		t.Fatalf("Chtimes(go.mod): %v", err)
	}

	_, fromCache3, err := loadGraph(opts, patterns, &stderr)
	if err != nil {
		t.Fatalf("loadGraph (after touching go.mod): %v", err)
	}
	if fromCache3 {
		t.Error("loadGraph (after touching go.mod) fromCache = true, want false (cache must be stale)")
	}
}
