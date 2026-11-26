package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/store"
)

// errTestGoModUntidy stands in for the real `go: updates to go.mod needed`
// failure graph.Load reports for an untidy go.mod (issue #163).
var errTestGoModUntidy = errors.New("go: updates to go.mod needed; to update it: go mod tidy")

// lockedBuffer is an io.Writer a Serve loop's own write path and the test's
// polling reader can use concurrently.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// showMessages decodes every window/showMessage notification written so far.
func (b *lockedBuffer) showMessages(t *testing.T) []protocol.ShowMessageParams {
	t.Helper()
	var got []protocol.ShowMessageParams
	for frame := range strings.SplitSeq(b.String(), "Content-Length:") {
		_, body, ok := strings.Cut(frame, "\r\n\r\n")
		if !ok {
			continue
		}
		var m struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(body), &m); err != nil || m.Method != protocol.MethodWindowShowMessage {
			continue
		}
		var p protocol.ShowMessageParams
		if err := protocol.Unmarshal(m.Params, &p); err != nil {
			t.Fatalf("unmarshal showMessage params: %v", err)
		}
		got = append(got, p)
	}
	return got
}

// loadFailureFixture is a Server that has been initialized over
// testdata/module while graphLoad fails until succeed is set.
type loadFailureFixture struct {
	s       *Server
	root    string
	out     *lockedBuffer
	succeed atomic.Bool
}

// newLoadFailureFixture initializes a Server whose first graphLoad fails with
// errTestGoModUntidy and whose later ones (after f.succeed is set) succeed.
//
// A facts index matching testdata/module is pre-built at the HOME-sandboxed
// location tryWarmOpen resolves, so a successful retry warm-opens it instead
// of falling through to buildIndex, which would launch this test binary
// itself as the indexer subprocess (see
// TestHandleInitialize_ReturnsBeforeGraphLoadCompletes).
func newLoadFailureFixture(t *testing.T) *loadFailureFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	dbPath := indexDBFile(root)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		t.Fatalf("mkdir index dir: %v", err)
	}
	cas, err := store.OpenCAS(casDir(root))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	buildTestIndexDB(t, snap, dbPath, cas)

	f := &loadFailureFixture{root: root, out: &lockedBuffer{}}
	orig := graphLoad
	t.Cleanup(func() { graphLoad = orig })
	graphLoad = func(graph.Options, ...string) (*graph.Snapshot, error) {
		if f.succeed.Load() {
			return snap, nil
		}
		return nil, errTestGoModUntidy
	}

	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
	f.s = New(rpcServer, Options{Logger: newTestLogger(t)})

	// Cleanups run last-in first-out: the Serve drain (which waits for any
	// in-flight loadWorkspaceAsync) must finish before the engine and the
	// warm-opened index handle are torn down.
	stopWorkspaceEngineOnCleanup(t, f.s)
	t.Cleanup(func() {
		if idx := f.s.idx.Load(); idx != nil {
			_ = idx.db.Close()
		}
	})
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		_ = rpcServer.Serve(context.Background(), pr, f.out)
		close(done)
	}()
	t.Cleanup(func() {
		_ = pw.Close()
		<-done
	})

	// Initialize through the wire rather than by calling handleInitialize
	// directly: the handler's later Notify calls read the rpc.Server's conn,
	// which Serve installs on its own goroutine, and only a request served
	// by Serve itself (whose response the test then observes) gives those
	// accesses a happens-before edge.
	params := mustMarshal(t, &protocol.InitializeParams{
		WorkspaceFoldersInitializeParams: protocol.WorkspaceFoldersInitializeParams{
			WorkspaceFolders: protocol.NewNullable([]protocol.WorkspaceFolder{{URI: uri.File(root), Name: "module"}}),
		},
	})
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": protocol.MethodInitialize, "params": params})
	if err != nil {
		t.Fatalf("marshal initialize request: %v", err)
	}
	if _, err := fmt.Fprintf(pw, "Content-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		t.Fatalf("write initialize request: %v", err)
	}
	waitFor(t, "initialize response", func() bool { return strings.Contains(f.out.String(), `"result"`) })
	return f
}

// waitFor polls cond until it holds or 10s pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// checkLoadFailedError asserts err is the RequestFailed error carrying the
// graph load failure, rather than an empty result or a timeout.
func checkLoadFailedError(t *testing.T, name string, err error) {
	t.Helper()
	checkIndexUnavailableError(t, name, err)
	if !strings.Contains(err.Error(), errTestGoModUntidy.Error()) {
		t.Errorf("%s: error %q does not mention the load failure %q", name, err, errTestGoModUntidy)
	}
}

// TestWaitWorkspace_ReturnsPromptlyAfterGraphLoadFailure pins issue #163: a
// failed initial graph load must release waitWorkspace callers instead of
// parking them until their request context times out.
func TestWaitWorkspace_ReturnsPromptlyAfterGraphLoadFailure(t *testing.T) {
	f := newLoadFailureFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	got := make(chan *workspace, 1)
	go func() { got <- f.s.waitWorkspace(ctx) }()

	select {
	case ws := <-got:
		if ws != nil {
			t.Fatalf("waitWorkspace = %v, want nil after a failed load", ws)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waitWorkspace still blocked 10s after the graph load failed")
	}
	if ctx.Err() != nil {
		t.Fatalf("waitWorkspace returned only because ctx ended: %v", ctx.Err())
	}

	// A caller arriving after the failure must not block either.
	if ws := f.s.waitWorkspace(ctx); ws != nil {
		t.Fatalf("late waitWorkspace = %v, want nil", ws)
	}
}

// TestGraphLoadFailure_RequestsFailFastWithLoadError verifies references and
// definition answer with RequestFailed carrying the load error, immediately,
// rather than blocking for the request timeout and returning null.
func TestGraphLoadFailure_RequestsFailFastWithLoadError(t *testing.T) {
	f := newLoadFailureFixture(t)
	waitFor(t, "load failure to be recorded", func() bool { return f.s.wsLoadErr.Load() != nil })

	file := filepath.Join(f.root, "greet", "greet.go")
	pos := protocol.Position{Line: 3, Character: 6}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	t.Run("references", func(t *testing.T) {
		result, err := f.s.handleReferences(ctx, mustMarshal(t, &protocol.ReferenceParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
				Position:     pos,
			},
		}))
		checkLoadFailedError(t, "references", err)
		if result != nil {
			t.Errorf("references: result = %#v, want nil", result)
		}
	})

	t.Run("definition", func(t *testing.T) {
		result, err := f.s.handleDefinition(ctx, mustMarshal(t, &protocol.DefinitionParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
				Position:     pos,
			},
		}))
		checkLoadFailedError(t, "definition", err)
		if result != nil {
			t.Errorf("definition: result = %#v, want nil", result)
		}
	})

	if ctx.Err() != nil {
		t.Fatalf("requests returned only because ctx ended: %v", ctx.Err())
	}
}

// TestGraphLoadFailure_ShowsErrorMessageToClient verifies the failure reaches
// the user as a window/showMessage of type Error naming the cause and the
// recovery step, not just the server log.
func TestGraphLoadFailure_ShowsErrorMessageToClient(t *testing.T) {
	f := newLoadFailureFixture(t)

	var msgs []protocol.ShowMessageParams
	waitFor(t, "showMessage to reach the client", func() bool {
		msgs = f.out.showMessages(t)
		return len(msgs) > 0
	})
	if len(msgs) != 1 {
		t.Fatalf("got %d showMessage notifications, want 1: %+v", len(msgs), msgs)
	}
	m := msgs[0]
	if m.Type != protocol.MessageTypeError {
		t.Errorf("showMessage type = %v, want Error", m.Type)
	}
	for _, want := range []string{errTestGoModUntidy.Error(), "go mod tidy", "go.mod"} {
		if !strings.Contains(m.Message, want) {
			t.Errorf("showMessage %q does not contain %q", m.Message, want)
		}
	}
}

// TestGraphLoadFailure_GoModChangeRetriesLoad verifies fixing go.mod and
// saving it (a watched-files change) retries the whole initial load, installs
// the workspace, and clears the recorded failure.
func TestGraphLoadFailure_GoModChangeRetriesLoad(t *testing.T) {
	f := newLoadFailureFixture(t)
	waitFor(t, "load failure to be recorded", func() bool { return f.s.wsLoadErr.Load() != nil })
	if f.s.workspace() != nil {
		t.Fatal("workspace installed despite the failed load")
	}

	// A change that is not module-structural must not trigger a retry.
	if err := f.s.handleDidChangeWatchedFiles(context.Background(), mustMarshal(t, &protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{URI: uri.File(filepath.Join(f.root, "greet", "greet.go")), Type: protocol.FileChangeTypeChanged}},
	})); err != nil {
		t.Fatalf("handleDidChangeWatchedFiles(greet.go): %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if f.s.workspace() != nil {
		t.Fatal("a .go change retried the load; only module files should")
	}

	f.succeed.Store(true)
	if err := f.s.handleDidChangeWatchedFiles(context.Background(), mustMarshal(t, &protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{URI: uri.File(filepath.Join(f.root, "go.mod")), Type: protocol.FileChangeTypeChanged}},
	})); err != nil {
		t.Fatalf("handleDidChangeWatchedFiles(go.mod): %v", err)
	}

	waitFor(t, "workspace to install after the go.mod change", func() bool { return f.s.workspace() != nil })
	if e := f.s.wsLoadErr.Load(); e != nil {
		t.Errorf("wsLoadErr = %+v after a successful retry, want nil", e)
	}
	waitFor(t, "retry guard to reset", func() bool { return !f.s.wsLoadRetrying.Load() })
}
