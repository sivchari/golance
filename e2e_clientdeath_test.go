package golance_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// clientDeathWatchInterval overrides golance's own client-liveness poll via
// GOLANCE_CLIENT_WATCH_INTERVAL (see internal/server/clientwatch.go), so
// this test observes a shutdown within seconds instead of waiting out the
// real 10s default.
const clientDeathWatchInterval = 1 * time.Second

// clientDeathExitBudget bounds how long this test waits for golance to
// exit after its watched client process dies: a generous CI margin over
// clientDeathWatchInterval, but comfortably under cmd/golance's own 30s
// os.Exit backstop (clientGoneExitTimeout) — this test's whole point is
// proving the graceful path fires well before that backstop would.
const clientDeathExitBudget = 20 * time.Second

// clientDeathInitializeID is the JSON-RPC id this test's own initialize
// request uses, so drainClientDeathStdout can recognize its response among
// golance's other stdout traffic.
const clientDeathInitializeID = `"1"`

// TestE2E_ServerExitsWhenClientProcessDies verifies golance shuts itself
// down when the process named by initialize's processId dies, even though
// golance's own stdin never sees EOF (a surviving child of the dead client
// could otherwise keep stdin's write end open forever — see
// internal/server/clientwatch.go's package doc). It asserts the graceful
// path (internal/server.watchClientProcess -> Options.ClientGone) fired,
// not cmd/golance's 30s os.Exit backstop.
func TestE2E_ServerExitsWhenClientProcessDies(t *testing.T) {
	if testing.Short() {
		t.Skip("-short set; skipping e2e")
	}

	bin := buildGolanceBinary(t)
	root := writeClientDeathModule(t)

	// Stands in for the real editor process golance's initialize processId
	// names: a process this test fully controls the death of, independent
	// of golance's own lifecycle.
	helper := exec.Command("sleep", "300")
	if err := helper.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	t.Cleanup(func() { _ = helper.Process.Kill(); _ = helper.Wait() })
	helperPID := clientDeathPID(t, helper.Process.Pid)

	fakeHome := t.TempDir()
	stderrPath := filepath.Join(fakeHome, "golance.stderr")
	stderrFile, err := os.Create(filepath.Clean(stderrPath))
	if err != nil {
		t.Fatalf("create stderr log: %v", err)
	}
	defer func() { _ = stderrFile.Close() }()

	cmd := runGolance(bin)
	cmd.Dir = root
	cmd.Env = append(e2eEnv(t, fakeHome), "GOLANCE_CLIENT_WATCH_INTERVAL="+clientDeathWatchInterval.String())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = stderrFile
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	// Deliberately never closed for the whole test: golance must exit even
	// though stdin's write end stays open, exactly as it would with a
	// surviving child of a dead editor.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start golance: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = cmd.Wait()
		if t.Failed() {
			logFileContent(t, "golance stderr", stderrPath)
		}
	})

	performClientDeathInitialize(t, stdin, stdout, root, helperPID)

	helperKilledAt := time.Now()
	if err := helper.Process.Kill(); err != nil {
		t.Fatalf("kill helper process: %v", err)
	}
	_ = helper.Wait()

	awaitClientDeathExitAndAssertLogs(t, cmd, stderrPath, helperKilledAt)
}

// writeClientDeathModule writes a minimal Go module: this test only needs a
// valid workspace root for initialize to accept, not any cross-reference
// behavior.
func writeClientDeathModule(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	writeE2EFile(t, root, "go.mod", "module example.com/clientdeath\n\ngo 1.23\n")
	writeE2EFile(t, root, "main.go", "package main\n\nfunc main() {}\n")
	return root
}

// clientDeathPID converts pid (an OS process id, always non-negative) to
// the int32 initialize's processId field requires. The bound check is the
// in-range edge gosec's G115 needs to see for the int -> int32 narrowing.
func clientDeathPID(t *testing.T, pid int) int32 {
	t.Helper()
	if pid >= 0 && pid <= math.MaxInt32 {
		return int32(pid)
	}
	t.Fatalf("pid %d out of int32 range", pid)
	return 0
}

// performClientDeathInitialize builds and sends an initialize request
// naming pid as initialize's processId, then blocks until golance answers
// it (or e2eRequestBudget elapses). handleInitialize starts
// watchClientProcess synchronously before that response is sent (see its
// own doc), so once this returns, golance is guaranteed to already be
// watching pid.
func performClientDeathInitialize(t *testing.T, stdin io.Writer, stdout io.Reader, root string, pid int32) {
	t.Helper()

	initDone := make(chan error, 1)
	go drainClientDeathStdout(bufio.NewReaderSize(stdout, 1<<20), initDone)

	params := &protocol.InitializeParams{
		ProcessID: &pid,
		WorkspaceFoldersInitializeParams: protocol.WorkspaceFoldersInitializeParams{
			WorkspaceFolders: protocol.NewNullable([]protocol.WorkspaceFolder{
				{URI: uri.File(root), Name: filepath.Base(root)},
			}),
		},
	}
	b, err := protocol.Marshal(params)
	if err != nil {
		t.Fatalf("marshal initialize params: %v", err)
	}
	raw, err := json.Marshal(message{JSONRPC: jsonrpcVersion, ID: json.RawMessage(clientDeathInitializeID), Method: protocol.MethodInitialize, Params: b})
	if err != nil {
		t.Fatalf("marshal initialize request: %v", err)
	}
	if err := newFrameWriter(stdin).write(raw); err != nil {
		t.Fatalf("write initialize request: %v", err)
	}

	select {
	case err := <-initDone:
		if err != nil {
			t.Fatalf("initialize: %v", err)
		}
	case <-time.After(e2eRequestBudget):
		t.Fatalf("golance did not respond to initialize within %s", e2eRequestBudget)
	}
}

// drainClientDeathStdout reads and discards every frame from r (golance's
// stdout) for the life of the process, reporting on done exactly once: the
// initialize response's error (nil on success) once clientDeathInitializeID
// arrives, or the read error if golance exits before ever answering it.
func drainClientDeathStdout(r *bufio.Reader, done chan<- error) {
	reported := false
	for {
		raw, err := readFrame(r)
		if err != nil {
			if !reported {
				done <- err
			}
			return
		}
		if reported {
			continue
		}
		var m message
		if json.Unmarshal(raw, &m) != nil || string(m.ID) != clientDeathInitializeID {
			continue
		}
		reported = true
		if len(m.Error) > 0 {
			done <- fmt.Errorf("initialize error: %s", m.Error)
			continue
		}
		done <- nil
	}
}

// awaitClientDeathExitAndAssertLogs waits for cmd to exit on its own
// within clientDeathExitBudget of killedAt, then asserts golance's stderr
// log (at stderrPath) recorded the graceful client-death shutdown path
// (internal/server/clientwatch.go's "client process ... is gone" line)
// rather than cmd/golance's 30s os.Exit backstop ("forcing exit").
func awaitClientDeathExitAndAssertLogs(t *testing.T, cmd *exec.Cmd, stderrPath string, killedAt time.Time) {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-done:
	case <-time.After(clientDeathExitBudget):
		t.Fatalf("golance did not exit within %s of its watched client process dying", clientDeathExitBudget)
	}
	t.Logf("golance exited %s after its watched client process died", time.Since(killedAt))

	stderrBytes, err := os.ReadFile(filepath.Clean(stderrPath))
	if err != nil {
		t.Fatalf("read stderr log: %v", err)
	}
	stderr := string(stderrBytes)
	if !strings.Contains(stderr, "client process") || !strings.Contains(stderr, "is gone") {
		t.Fatalf("expected golance's stderr to log the client process going away, got:\n%s", stderr)
	}
	if strings.Contains(stderr, "forcing exit") {
		t.Fatalf("golance hit its 30s os.Exit backstop instead of the graceful shutdown path:\n%s", stderr)
	}
}
