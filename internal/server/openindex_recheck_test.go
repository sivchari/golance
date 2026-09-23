package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/store"
)

// refreshRequestObserver watches a live rpc.Server session's outbound
// frames for server-to-client requests and lets a test block until a
// specific method arrives, instead of polling a snapshot buffer against a
// fixed wall-clock deadline. The polling version raced two failure modes on
// a slower CI runner: the push's own goroutine could simply be scheduled
// late past the fixed deadline, and -- because that harness ran Serve to
// completion before the test ever triggered a push -- rpc.Server.Request's
// wait for a response used a session context (rpc.Server.Serve's
// sessionCtx) already canceled by the time Serve returned, so it always lost
// the race to nothing ever answering it once scheduling delay reached the
// deadline, logging "context canceled" for a write that (writeMessage
// always runs before Request's ctx select) had in fact already gone out.
// Keeping the session alive for this observer to read from sidesteps both:
// the write is observed the instant it happens, and the session ctx a real
// Request is still waiting on stays live throughout, matching a real
// long-running editor session instead of a synthetic one that dies before
// the async work it triggered ever runs.
type refreshRequestObserver struct {
	mu   sync.Mutex
	seen map[string]chan struct{}
}

func newRefreshRequestObserver() *refreshRequestObserver {
	return &refreshRequestObserver{seen: make(map[string]chan struct{})}
}

func (o *refreshRequestObserver) chanFor(method string) chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	c, ok := o.seen[method]
	if !ok {
		c = make(chan struct{}, 8)
		o.seen[method] = c
	}
	return c
}

// mark records that method's request frame arrived.
func (o *refreshRequestObserver) mark(method string) {
	c := o.chanFor(method)
	select {
	case c <- struct{}{}:
	default: // an unconsumed occurrence is already buffered; that is enough
	}
}

// discard drops any occurrence of method already observed but not yet
// waited for, so a later wait only succeeds on one that arrives strictly
// after this call -- the equivalent of the old syncBuffer.Reset, for
// "discard the pushes above; only the self-heal one below is this test's
// own subject".
func (o *refreshRequestObserver) discard(method string) {
	c := o.chanFor(method)
	select {
	case <-c:
	default:
	}
}

// wait blocks until method's request frame arrives or timeout elapses.
func (o *refreshRequestObserver) wait(t *testing.T, method string, timeout time.Duration) {
	t.Helper()
	select {
	case <-o.chanFor(method):
	case <-time.After(timeout):
		t.Fatalf("no %s within %s", method, timeout)
	}
}

// readRefreshRequests decodes Content-Length-framed JSON-RPC messages from
// r, reporting each one's method to obs, until r errors -- the session
// pipe closing at test cleanup, which ends this goroutine.
func readRefreshRequests(r io.Reader, obs *refreshRequestObserver) {
	br := bufio.NewReaderSize(r, 1<<16)
	for {
		body, err := readContentLengthFrame(br)
		if err != nil {
			return
		}
		var m struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(body, &m) == nil && m.Method != "" {
			obs.mark(m.Method)
		}
	}
}

// readContentLengthFrame reads one Content-Length-delimited message body
// from r. internal/rpc's own equivalent (readFrame) is unexported, so this
// mirrors just enough of its header parsing for a test-only reader of the
// same wire format.
func readContentLengthFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return nil, err
			}
			length = n
		}
	}
	if length < 0 {
		return nil, errors.New("rpc: missing Content-Length header")
	}
	body := make([]byte, length)
	_, err := io.ReadFull(r, body)
	return body, err
}

// readinessProbeMethod is an unregistered notification method used solely
// to synchronize with newCodeLensRefreshCaptureServer's own Serve goroutine
// (see sendReadinessProbe): dispatchNotification silently drops any
// notification with no registered handler, so sending this has no
// observable effect beyond proving Serve's read loop reached it.
const readinessProbeMethod = "$/golanceTestReadinessProbe"

// sendReadinessProbe writes one Content-Length-framed notification for
// readinessProbeMethod to w and blocks until it is fully written. io.Pipe's
// Write blocks until a Read fully consumes it, so this only returns once
// Serve's read loop has actually read from the pipe -- which happens after
// Serve sets its conn field, its very first action (see
// newCodeLensRefreshCaptureServer's own doc for why that ordering matters).
func sendReadinessProbe(t *testing.T, w io.Writer) {
	t.Helper()
	body := fmt.Appendf(nil, `{"jsonrpc":"2.0","method":%q}`, readinessProbeMethod)
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		t.Fatalf("write readiness probe header: %v", err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatalf("write readiness probe body: %v", err)
	}
}

// newCodeLensRefreshCaptureServer builds a workspace-only Server exactly
// like newTestServerNoIndex, except it is wired over a real rpc.Server whose
// session stays alive for the whole test: recheckOpenFilesAfterIndexReady's
// self-heal fires its refresh pushes via s.rpc.Go, which runs against
// rpc.Server.Serve's own session context, so that context must still be
// live when the push happens (see refreshRequestObserver's own doc) --
// unlike newNotifyCaptureServer's pattern (indexer_test.go) of running
// Serve to completion first, which only works for a push the test itself
// triggers synchronously, not one a background debounce fires later. Both
// pipes, and the reader goroutine draining the outbound one into obs, are
// torn down in t.Cleanup, after stopWorkspaceEngineOnCleanup's own cleanup
// (registered later, so it runs first) has stopped every engine that could
// still trigger a push.
func newCodeLensRefreshCaptureServer(t *testing.T) (s *Server, snap *graph.Snapshot, obs *refreshRequestObserver) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err = graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}

	inPR, inPW := io.Pipe()   // client-to-server: only ever fed sendReadinessProbe's one frame
	outPR, outPW := io.Pipe() // server-to-client: obs reads this live
	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
	obs = newRefreshRequestObserver()
	go readRefreshRequests(outPR, obs)
	serveDone := make(chan struct{})
	go func() {
		_ = rpcServer.Serve(context.Background(), inPR, outPW)
		close(serveDone)
	}()
	t.Cleanup(func() {
		_ = inPW.Close()  // Serve's read loop sees a clean EOF and returns
		<-serveDone       // ... and its session ctx cancels, unblocking any lingering Go-launched push
		_ = outPW.Close() // readRefreshRequests then sees an error and exits
	})
	// Serve's own conn field (rpc.Server.conn) is set as its very first
	// action, before its read loop starts; without waiting for that, a push
	// triggered moments later by this test can read it concurrently with
	// Serve's goroutine still writing it (caught by -race). Sending one
	// frame here and letting sendReadinessProbe's write block until Serve's
	// read loop actually consumes it reproduces the same happens-before
	// edge newNotifyCaptureServer's pattern gets from waiting for Serve to
	// return outright, without requiring this Serve call to ever return.
	sendReadinessProbe(t, inPW)

	s = New(rpcServer, Options{Logger: newTestLogger(t)})
	s.setWorkspace(root, snap)
	stopWorkspaceEngineOnCleanup(t, s)
	return s, snap, obs
}

// TestOpenIndexAfterBuild_RechecksOpenFilesWithoutEditOrSave is the
// regression test for B2: a file opened DURING a cold index build, and
// never subsequently edited or saved -- the one case drainDirty's own
// markDirty/takeDirty queue does not cover (see
// recheckOpenFilesAfterIndexReady's doc in indexer.go) -- must still get a
// fresh recheck the moment the index becomes ready, so navigation into a
// cross-package WORKSPACE import starts working without the user touching
// the file at all. depuse.go's UseGreet references greet.Greeting, a
// workspace (root) package: engineImporter's own doc explains why a ROOT
// import specifically stays unresolved for the whole span of a cold build.
func TestOpenIndexAfterBuild_RechecksOpenFilesWithoutEditOrSave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		s, snap := newTestServerNoIndex(t)
		root := s.workspace().root

		file := snap.Packages["example.com/servermod/depuse"].GoFiles[0]
		text, err := os.ReadFile(filepath.Clean(file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		openDoc(t, s, file, string(text))
		synctest.Wait() // let the initial (cold) recheck complete

		pos := identPositionIn(t, file, text, "Text", 1) // g.Text in UseGreet

		before, err := s.handleHover(context.Background(), mustMarshal(t, &protocol.HoverParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
				Position:     pos,
			},
		}))
		if err != nil {
			t.Fatalf("handleHover before index ready: %v", err)
		}
		t.Logf("hover before index ready: %#v", before)

		dbPath := indexDBFile(root)
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
			t.Fatalf("mkdir index dir: %v", err)
		}
		cas, err := store.OpenCAS(casDir(root))
		if err != nil {
			t.Fatalf("store.OpenCAS: %v", err)
		}
		buildTestIndexDB(t, snap, dbPath, cas)

		if s.openIndexAfterBuild(context.Background(), dbPath, nil, "", 0) {
			t.Fatal("openIndexAfterBuild locked = true, want false")
		}
		idx := s.idx.Load()
		if idx == nil {
			t.Fatal("s.idx is nil after openIndexAfterBuild")
		}
		t.Cleanup(func() { _ = idx.db.Close() })

		synctest.Wait() // let recheckOpenFilesAfterIndexReady's own recheck complete

		after, err := s.handleHover(context.Background(), mustMarshal(t, &protocol.HoverParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
				Position:     pos,
			},
		}))
		if err != nil {
			t.Fatalf("handleHover after index ready: %v", err)
		}
		hover, ok := after.(*protocol.Hover)
		if !ok || hover == nil {
			t.Fatalf("handleHover after index ready and recheckOpenFilesAfterIndexReady = %#v, want a real hover for Text without any edit or save", after)
		}
		md, ok := hover.Contents.(*protocol.MarkupContent)
		if !ok {
			t.Fatalf("hover.Contents = %#v, want *protocol.MarkupContent", hover.Contents)
		}
		t.Logf("hover after index ready: %s", md.Value)
		if want := "Text"; !contains(md.Value, want) {
			t.Fatalf("hover content = %q, want it to contain %q", md.Value, want)
		}
	})
}

// TestOpenIndexAfterBuild_RechecksCodeLensOnTestFile is
// TestOpenIndexAfterBuild_RechecksOpenFilesWithoutEditOrSave's own scenario
// for a _test.go file's own code lens rather than a hover on a
// cross-package identifier: codelens_test.go's TestAdd resolves its sole
// parameter's type through "testing", a dependency import that (like a ROOT
// import, see depCacheHolder.importer's own doc) stays unresolved for the
// whole span of a cold index build (coldGateSource) -- degrading
// matchTestFunc's *testing.T signature check (internal/langfeat/
// codelens.go) to false and so silently dropping the "run test" lens,
// exactly as an interactive textDocument/codeLens request racing the
// workspace's still-building facts index would. This pins that
// recheckOpenFilesAfterIndexReady's self-heal (already proven for hover
// above) covers code lens too.
func TestOpenIndexAfterBuild_RechecksCodeLensOnTestFile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		s, snap := newTestServerNoIndex(t)
		s.setCodeLensesEnabled(map[codeLensSource]bool{codeLensTest: true})
		root := s.workspace().root

		testFile := filepath.Join(root, "codelens", "codelens_test.go")
		text, err := os.ReadFile(filepath.Clean(testFile))
		if err != nil {
			t.Fatalf("read %s: %v", testFile, err)
		}
		openDoc(t, s, testFile, string(text))
		synctest.Wait() // let the initial (cold) recheck complete

		before := requestCodeLens(t, s, testFile)
		t.Logf("codeLens before index ready: %+v", before)

		dbPath := indexDBFile(root)
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
			t.Fatalf("mkdir index dir: %v", err)
		}
		cas, err := store.OpenCAS(casDir(root))
		if err != nil {
			t.Fatalf("store.OpenCAS: %v", err)
		}
		buildTestIndexDB(t, snap, dbPath, cas)

		if s.openIndexAfterBuild(context.Background(), dbPath, nil, "", 0) {
			t.Fatal("openIndexAfterBuild locked = true, want false")
		}
		idx := s.idx.Load()
		if idx == nil {
			t.Fatal("s.idx is nil after openIndexAfterBuild")
		}
		t.Cleanup(func() { _ = idx.db.Close() })

		synctest.Wait() // let recheckOpenFilesAfterIndexReady's own recheck complete

		after := requestCodeLens(t, s, testFile)
		found := false
		for i := range after {
			if after[i].Command.Title == "run test" {
				found = true
			}
		}
		if !found {
			t.Fatalf("codeLens after index ready and recheckOpenFilesAfterIndexReady = %+v, want a %q entry", after, "run test")
		}
	})
}

// TestOpenIndexAfterBuild_PushesCodeLensAndInlayHintRefreshOnSelfHeal proves
// the other half of TestOpenIndexAfterBuild_RechecksCodeLensOnTestFile's own
// fix: a client that declared workspace.codeLens.refreshSupport (or
// workspace.inlayHint.refreshSupport) must not be left to poll for the
// self-healed "run test" lens -- publishDiagnostics (see diagnostics.go)
// must push workspace/codeLens/refresh (and workspace/inlayHint/refresh)
// once recheckOpenFilesAfterIndexReady's own debounce-armed recheck fires,
// entirely on its own, with no client interaction forcing a fresh check in
// between (see check.Engine.InvalidateDependency's own doc: it arms the
// same debounce-triggered recheck+publish path every ordinary edit already
// goes through).
//
// Deliberately not synctest-based, unlike this file's other tests: the
// debounce timer this exercises is check.Engine's own time.AfterFunc, and
// under synctest's fake clock it only fires once something else (a second
// Get, e.g. requestCodeLens) forces a recheck first -- proven against real
// time instead, so this test's "self-heal" claim is not an artifact of
// synctest's own scheduling.
func TestOpenIndexAfterBuild_PushesCodeLensAndInlayHintRefreshOnSelfHeal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, snap, obs := newCodeLensRefreshCaptureServer(t)
	s.codeLensRefreshSupport.Store(true)
	s.inlayHintRefreshSupport.Store(true)
	s.setCodeLensesEnabled(map[codeLensSource]bool{codeLensTest: true})
	root := s.workspace().root

	const waitTimeout = 30 * time.Second

	testFile := filepath.Join(root, "codelens", "codelens_test.go")
	text, err := os.ReadFile(filepath.Clean(testFile))
	if err != nil {
		t.Fatalf("read %s: %v", testFile, err)
	}
	openDoc(t, s, testFile, string(text))

	// Registers testFile's directory with the engine (see
	// check.Engine.getUnit), so InvalidateDependency below -- which only
	// arms a debounce for a directory the engine already knows about -- has
	// something to invalidate.
	requestCodeLens(t, s, testFile)
	obs.wait(t, protocol.MethodWorkspaceCodeLensRefresh, waitTimeout)
	// Discard the pushes above; only the self-heal ones below are this
	// test's own subject (publishDiagnostics pushes both refreshes together,
	// so the codeLens wait above may have been accompanied by an inlayHint
	// one too).
	obs.discard(protocol.MethodWorkspaceCodeLensRefresh)
	obs.discard(protocol.MethodWorkspaceInlayHintRefresh)

	dbPath := indexDBFile(root)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		t.Fatalf("mkdir index dir: %v", err)
	}
	cas, err := store.OpenCAS(casDir(root))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	buildTestIndexDB(t, snap, dbPath, cas)

	if s.openIndexAfterBuild(context.Background(), dbPath, nil, "", 0) {
		t.Fatal("openIndexAfterBuild locked = true, want false")
	}
	idx := s.idx.Load()
	if idx == nil {
		t.Fatal("s.idx is nil after openIndexAfterBuild")
	}
	t.Cleanup(func() { _ = idx.db.Close() })

	obs.wait(t, protocol.MethodWorkspaceCodeLensRefresh, waitTimeout)
	obs.wait(t, protocol.MethodWorkspaceInlayHintRefresh, waitTimeout)
}
