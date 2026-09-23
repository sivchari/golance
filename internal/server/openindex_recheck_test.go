package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/store"
)

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
