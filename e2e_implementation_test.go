package golance_test

import (
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// e2eImplLocs records the exact 0-based positions TestE2E_Implementation
// queries, captured while the synthetic module is written so nothing is
// re-parsed at query time.
type e2eImplLocs struct {
	greetFile string // lib/greet/greet.go
	greetSrc  string

	appFile string // app/app.go
	appSrc  string

	interfaceNamePos   protocol.Position // "Greeter" at its own declaration
	interfaceMethodPos protocol.Position // "Greet" inside the interface body
	useSitePos         protocol.Position // "Greeter" in "var G greet.Greeter"
	concreteMethodPos  protocol.Position // "Greet" at Human's own method declaration
}

func writeE2EImplModule(t *testing.T) (string, e2eImplLocs) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	var locs e2eImplLocs
	writeE2EFile(t, root, "go.mod", "module example.com/e2eimpl\n\ngo 1.23\n")

	const greetSrc = `package greet

// Greeter can greet.
type Greeter interface {
	Greet() string
}
`
	locs.greetFile = writeE2EFile(t, root, "lib/greet/greet.go", greetSrc)
	locs.greetSrc = greetSrc

	const appSrc = `package app

import "example.com/e2eimpl/lib/greet"

// Human implements Greeter via a value receiver.
type Human struct{}

// Greet returns a greeting.
func (h Human) Greet() string {
	return "hello from Human"
}

// Robot implements Greeter via a pointer receiver.
type Robot struct{}

// Greet returns a greeting.
func (r *Robot) Greet() string {
	return "hello from Robot"
}

// G is typed as greet.Greeter without holding a concrete value.
var G greet.Greeter
`
	locs.appFile = writeE2EFile(t, root, "app/app.go", appSrc)
	locs.appSrc = appSrc

	locs.interfaceNamePos = mustPos(t, greetSrc, "type Greeter interface", "Greeter")
	locs.interfaceMethodPos = mustPos(t, greetSrc, "Greet() string", "Greet")
	locs.useSitePos = mustPos(t, appSrc, "var G greet.Greeter", "Greeter")
	locs.concreteMethodPos = mustPos(t, appSrc, "func (h Human) Greet() string {", "Greet")

	return root, locs
}

// TestE2E_Implementation drives a real golance binary over stdio, covering
// textDocument/implementation (internal/server/handlers_xref.go's
// handleImplementation) from the three cursor positions gopls supports for
// an interface: the interface's own name at declaration, an interface
// method name, and a use of the interface type elsewhere. Greeter has two
// implementers -- one via a value receiver (Human), one via a pointer
// receiver (Robot) -- so every subtest also exercises both receiver kinds.
func TestE2E_Implementation(t *testing.T) {
	skipUnlessE2E(t)

	root, locs := writeE2EImplModule(t)
	c := startClient(t, root)
	c.initialize(t, root)

	c.openFile(t, locs.appFile)
	c.waitForIndexReady(t)

	t.Run("interface_name_lists_both_receivers", func(t *testing.T) {
		checkE2EImplementationListsBothReceivers(t, c, &locs, locs.greetFile, locs.interfaceNamePos)
	})

	t.Run("interface_method_name_lists_both_receiver_methods", func(t *testing.T) {
		checkE2EImplementationListsBothReceivers(t, c, &locs, locs.greetFile, locs.interfaceMethodPos)
	})

	t.Run("use_site_lists_both_receivers", func(t *testing.T) {
		checkE2EImplementationListsBothReceivers(t, c, &locs, locs.appFile, locs.useSitePos)
	})

	t.Run("concrete_method_name_lists_interface_method", func(t *testing.T) {
		result := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentImplementation, &protocol.ImplementationParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.appFile)},
				Position:     locs.concreteMethodPos,
			},
		}, e2eIndexBudget)
		if len(result) != 1 {
			t.Fatalf("implementation returned %d locations, want 1: %+v", len(result), result)
		}
		wantLine := locs.interfaceMethodPos.Line
		if gotPath := result[0].URI.FsPath(); gotPath != locs.greetFile || result[0].Range.Start.Line != wantLine {
			t.Errorf("implementation = %s:%d, want %s:%d", gotPath, result[0].Range.Start.Line, locs.greetFile, wantLine)
		}
	})
}

// checkE2EImplementationListsBothReceivers issues textDocument/implementation
// at (file, pos) and asserts the result covers both Human.Greet and
// Robot.Greet, both declared in app.go regardless of which file the query
// itself targets.
func checkE2EImplementationListsBothReceivers(t *testing.T, c *lspClient, locs *e2eImplLocs, file string, pos protocol.Position) {
	t.Helper()
	result := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentImplementation, &protocol.ImplementationParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}, e2eIndexBudget)
	if len(result) != 2 {
		t.Fatalf("implementation returned %d locations, want 2 (Human and Robot): %+v", len(result), result)
	}
	for _, loc := range result {
		if gotPath := loc.URI.FsPath(); gotPath != locs.appFile {
			t.Errorf("implementation location file = %s, want %s", gotPath, locs.appFile)
		}
	}
}
