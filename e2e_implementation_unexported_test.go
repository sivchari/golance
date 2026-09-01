package golance_test

import (
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// e2eDBerLocs records the exact 0-based positions
// TestE2E_Implementation_UnexportedImplementer queries, captured while the
// synthetic module is written so nothing is re-parsed at query time.
type e2eDBerLocs struct {
	dberFile string // dber/dber.go
	dbFile   string // dber/db.go

	interfaceNamePos protocol.Position // "DBer" at its own declaration
	methodNamePos    protocol.Position // "NewDB" inside the interface body
}

func writeE2EDBerModule(t *testing.T) (string, e2eDBerLocs) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	var locs e2eDBerLocs
	writeE2EFile(t, root, "go.mod", "module example.com/e2edber\n\ngo 1.23\n")

	const dberSrc = `package dber

// DBer is the exported entry point a caller depends on.
type DBer interface {
	NewDB() error
}
`
	locs.dberFile = writeE2EFile(t, root, "dber/dber.go", dberSrc)

	const dbSrc = `package dber

// db is the unexported implementer: real code only ever sees it through
// the DBer interface NewDBer returns -- the dominant "unexported struct
// implements an exported interface, constructor returns the interface"
// pattern a real monorepo report traced "Go to Implementation" failing on.
type db struct{}

func (d *db) NewDB() error { return nil }

// NewDBer constructs a db and returns it as DBer.
func NewDBer() DBer { return &db{} }
`
	locs.dbFile = writeE2EFile(t, root, "dber/db.go", dbSrc)

	locs.interfaceNamePos = mustPos(t, dberSrc, "type DBer interface", "DBer")
	locs.methodNamePos = mustPos(t, dberSrc, "NewDB() error", "NewDB")

	return root, locs
}

// TestE2E_Implementation_UnexportedImplementer drives a real golance binary
// over stdio, covering textDocument/implementation on an interface whose
// ONLY implementer is unexported (the exact DBer shape from the production
// report internal/xref/implementation.go's own doc describes): export data
// only ever carries exported package-scope objects, so the pre-fix
// decode-then-types.Implements confirmation could never see db at all. Both
// cursor positions the real report exercised are covered: the interface's
// own name, and its method name.
func TestE2E_Implementation_UnexportedImplementer(t *testing.T) {
	skipUnlessE2E(t)

	root, locs := writeE2EDBerModule(t)
	c := startClient(t, root)
	c.initialize(t, root)

	c.openFile(t, locs.dberFile)
	c.waitForIndexReady(t)

	t.Run("interface_name", func(t *testing.T) {
		result := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentImplementation, &protocol.ImplementationParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.dberFile)},
				Position:     locs.interfaceNamePos,
			},
		}, e2eIndexBudget)
		if len(result) != 1 {
			t.Fatalf("implementation(DBer) returned %d locations, want 1 (the unexported db type): %+v", len(result), result)
		}
		if gotPath := result[0].URI.FsPath(); gotPath != locs.dbFile {
			t.Errorf("implementation(DBer) file = %s, want %s (db.go)", gotPath, locs.dbFile)
		}
	})

	t.Run("method_name", func(t *testing.T) {
		result := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentImplementation, &protocol.ImplementationParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.dberFile)},
				Position:     locs.methodNamePos,
			},
		}, e2eIndexBudget)
		if len(result) != 1 {
			t.Fatalf("implementation(DBer.NewDB) returned %d locations, want 1 (db's own NewDB method): %+v", len(result), result)
		}
		if gotPath := result[0].URI.FsPath(); gotPath != locs.dbFile {
			t.Errorf("implementation(DBer.NewDB) file = %s, want %s (db.go)", gotPath, locs.dbFile)
		}
	})
}
