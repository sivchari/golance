package server

import (
	"context"
	"io"
	"os"
	"path/filepath"
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

// syncBuffer is a concurrency-safe byte buffer, for a test that polls its
// content from its own goroutine while a detached background push (see
// rpc.Server.Go) may still be writing to it -- unlike this file's
// synctest-based tests, which only ever inspect a shared buffer at a point
// synctest.Wait has already proven no other goroutine is running.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// newCodeLensRefreshCaptureServer builds a workspace-only Server exactly
// like newTestServerNoIndex, except it is wired over a real rpc.Server whose
// outbound requests land in the returned buffer. Mirrors
// newNotifyCaptureServer's own pattern (indexer_test.go): Serve is run to
// completion first, over an already-closed pipe, so its conn -- installed
// as Serve's very first action and never cleared once Serve returns -- has
// a clean happens-before edge into every later write, with nothing left
// concurrent; Serve's read loop returning early does not stop later
// Notify/Request calls from finding a live connection to write on.
func newCodeLensRefreshCaptureServer(t *testing.T) (s *Server, snap *graph.Snapshot, out *syncBuffer) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err = graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}

	out = &syncBuffer{}
	pr, pw := io.Pipe()
	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
	done := make(chan struct{})
	go func() {
		_ = rpcServer.Serve(context.Background(), pr, out)
		close(done)
	}()
	if err := pw.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	<-done

	s = New(rpcServer, Options{Logger: newTestLogger(t)})
	s.setWorkspace(root, snap)
	stopWorkspaceEngineOnCleanup(t, s)
	return s, snap, out
}

// waitForServerRequest polls out (see newCodeLensRefreshCaptureServer) until
// it contains a request for method, up to a bounded real-time deadline --
// the same "poll for an eventually-consistent answer" idiom the e2e suite's
// own pollCodeLensE2E already uses (see e2e_codelens_test.go), applied here
// to a server-initiated push instead of a client-initiated request/response.
func waitForServerRequest(t *testing.T, out *syncBuffer, method string) {
	t.Helper()
	want := `"method":"` + method + `"`
	deadline := time.Now().Add(2 * time.Second)
	for {
		if strings.Contains(out.String(), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s within 2s: %q", method, out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
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
	s, snap, out := newCodeLensRefreshCaptureServer(t)
	s.codeLensRefreshSupport.Store(true)
	s.inlayHintRefreshSupport.Store(true)
	s.setCodeLensesEnabled(map[codeLensSource]bool{codeLensTest: true})
	root := s.workspace().root

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
	waitForServerRequest(t, out, protocol.MethodWorkspaceCodeLensRefresh)
	out.Reset() // discard the pushes above; only the self-heal one below is this test's own subject

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

	waitForServerRequest(t, out, protocol.MethodWorkspaceCodeLensRefresh)
	waitForServerRequest(t, out, protocol.MethodWorkspaceInlayHintRefresh)
}
