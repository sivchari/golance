package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/xref"
)

// newRootFieldTypeServer builds the regression workspace for a struct field
// typed as an interface declared in another ROOT (workspace) package —
// testdata/rootfieldtype/usecase.U.s is a repository.Store field, and
// U.Call invokes it through the interface (u.s.Get(1)). Both repository and
// usecase are root packages, so the facts index covers both.
func newRootFieldTypeServer(t *testing.T) (s *Server, usecaseFile, repositoryFile string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "rootfieldtype"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}

	relative := RelativeIndexPaths(root)
	db, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("db.Close: %v", err)
		}
	})
	cas, err := store.OpenCAS(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	if _, err := index.Build(context.Background(), snap, db, cas, &index.Options{RelativePaths: relative}); err != nil {
		t.Fatalf("index.Build: %v", err)
	}

	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
	s = New(rpcServer, Options{Logger: newTestLogger(t)})
	s.setWorkspace(root, snap)
	stopWorkspaceEngineOnCleanup(t, s)
	s.idx.Store(&indexState{db: db, cas: cas, resolver: xref.New(db, cas, snap, relative)})

	usecaseFile = filepath.Join(root, "usecase", "usecase.go")
	repositoryFile = filepath.Join(root, "repository", "repository.go")
	return s, usecaseFile, repositoryFile
}

// TestFactsIndexDeclaration_FieldTypeFastPath verifies dependencyDefinition
// resolves a struct field's interface type through factsIndexDeclaration --
// an O(1) facts index read -- rather than depcheck.Decl's much slower
// source-check of the whole target root package: phaseTimer's dominant
// phase must be "facts.Decl", never "depcheck.Decl", proving the source-check
// path was never reached.
func TestFactsIndexDeclaration_FieldTypeFastPath(t *testing.T) {
	s, usecaseFile, repositoryFile := newRootFieldTypeServer(t)

	data, err := os.ReadFile(usecaseFile)
	if err != nil {
		t.Fatalf("read usecase.go: %v", err)
	}
	pos := identPositionIn(t, usecaseFile, data, "Store", 1)

	cf := s.checkedFile(context.Background(), uri.File(usecaseFile), pos)
	if !cf.ok {
		t.Fatalf("checkedFile(usecase.go): not ok")
	}

	ctx, pt := withPhaseTimer(context.Background())
	loc, ok := s.dependencyDefinition(ctx, cf)
	if !ok {
		t.Fatalf("dependencyDefinition(Store field type): ok = false, want true")
	}
	if loc.File != repositoryFile {
		t.Errorf("dependencyDefinition(Store field type) = %+v, want a location in %s", loc, repositoryFile)
	}

	if name, _ := pt.dominant(); name != "facts.Decl" {
		t.Errorf("dominant phase = %q, want %q (depcheck.Decl must never engage for a root-package target the facts index already answers)", name, "facts.Decl")
	}
}

// TestHandleDefinition_FieldTypeInterface pins the same field-type position
// through the full handleDefinition path (not dependencyDefinition
// directly): the primary resolver.Definition call already answers this from
// the facts index (see internal/index/facts.go's addRefs, which records a
// ref at every info.Uses/info.Selections position, including a struct
// field's type), so this never even reaches dependencyDefinition in
// practice -- this test pins that end-to-end behavior so a regression that
// broke the primary path would be caught here, separately from
// TestFactsIndexDeclaration_FieldTypeFastPath's direct fallback coverage.
func TestHandleDefinition_FieldTypeInterface(t *testing.T) {
	s, usecaseFile, repositoryFile := newRootFieldTypeServer(t)

	data, err := os.ReadFile(usecaseFile)
	if err != nil {
		t.Fatalf("read usecase.go: %v", err)
	}
	pos := identPositionIn(t, usecaseFile, data, "Store", 1)

	result, err := s.handleDefinition(context.Background(), mustMarshal(t, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(usecaseFile)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleDefinition(Store field type): %v", err)
	}
	locs, ok := result.(protocol.LocationSlice)
	if !ok || len(locs) != 1 {
		t.Fatalf("handleDefinition(Store field type) = %#v, want a single Location", result)
	}
	if got := locs[0].URI.FsPath(); got != repositoryFile {
		t.Errorf("handleDefinition(Store field type) resolved to %s, want %s", got, repositoryFile)
	}
}

// TestHandleReferences_InterfaceFieldTypeAndMethodCallSite verifies
// References finds both a field-type use of an interface and a call site
// reached through a field of that interface type.
func TestHandleReferences_InterfaceFieldTypeAndMethodCallSite(t *testing.T) {
	s, usecaseFile, repositoryFile := newRootFieldTypeServer(t)

	storePos := identPositionIn(t, repositoryFile, mustRead(t, repositoryFile), "Store", 1)
	storeRefs, err := s.handleReferences(context.Background(), mustMarshal(t, &protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(repositoryFile)},
			Position:     storePos,
		},
		Context: protocol.ReferenceContext{IncludeDeclaration: false},
	}))
	if err != nil {
		t.Fatalf("handleReferences(Store): %v", err)
	}
	storeLocs, ok := storeRefs.(protocol.LocationSlice)
	if !ok || len(storeLocs) != 1 {
		t.Fatalf("handleReferences(Store) = %#v, want a single field-type reference", storeRefs)
	}
	if got := storeLocs[0].URI.FsPath(); got != usecaseFile {
		t.Errorf("handleReferences(Store) resolved to %s, want %s", got, usecaseFile)
	}

	getPos := identPositionIn(t, repositoryFile, mustRead(t, repositoryFile), "Get", 1)
	getRefs, err := s.handleReferences(context.Background(), mustMarshal(t, &protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(repositoryFile)},
			Position:     getPos,
		},
		Context: protocol.ReferenceContext{IncludeDeclaration: false},
	}))
	if err != nil {
		t.Fatalf("handleReferences(Store.Get): %v", err)
	}
	getLocs, ok := getRefs.(protocol.LocationSlice)
	if !ok || len(getLocs) != 1 {
		t.Fatalf("handleReferences(Store.Get) = %#v, want a single call-site reference", getRefs)
	}
	if got := getLocs[0].URI.FsPath(); got != usecaseFile {
		t.Errorf("handleReferences(Store.Get) resolved to %s, want %s", got, usecaseFile)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}
