package golance_test

import (
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// e2eRefsReverseLocs records the exact 0-based positions
// TestE2E_ReferencesOnConcreteMethodFindsInterfaceTypedCallSite queries,
// captured while the synthetic module is written so nothing is re-parsed at
// query time.
type e2eRefsReverseLocs struct {
	greetFile string // lib/greet/greet.go
	appFile   string // app/app.go

	concreteMethodDeclPos protocol.Position // "Greet" at Human's own method declaration
	interfaceCallPos      protocol.Position // "Greet" in CallGreeter's g.Greet()
}

// writeE2ERefsReverseModule writes a dependency-injection-shaped fixture: a
// constructor (NewGreeter) returns an interface (greet.Greeter), and every
// call site in the workspace reaches Human's Greet only through that
// interface-typed value -- the "daily pain in DI-heavy code" shape
// References invoked on a CONCRETE method's own declaration used to miss
// entirely, since a call through an interface-typed variable is recorded
// against the interface method's own SymbolID, not the concrete method's.
func writeE2ERefsReverseModule(t *testing.T) (string, e2eRefsReverseLocs) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	var locs e2eRefsReverseLocs
	writeE2EFile(t, root, "go.mod", "module example.com/e2erefsrev\n\ngo 1.23\n")

	const greetSrc = `package greet

// Greeter can greet.
type Greeter interface {
	Greet() string
}
`
	locs.greetFile = writeE2EFile(t, root, "lib/greet/greet.go", greetSrc)

	const appSrc = `package app

import "example.com/e2erefsrev/lib/greet"

// Human implements Greeter via a value receiver.
type Human struct{}

// Greet returns a greeting.
func (h Human) Greet() string {
	return "hello from Human"
}

// NewGreeter constructs a Human but returns it as the Greeter interface --
// the dependency-injection shape: every caller only ever sees a
// greet.Greeter, never a concrete Human.
func NewGreeter() greet.Greeter {
	return Human{}
}

// CallGreeter calls Greet only through the interface-typed value NewGreeter
// returns -- the only call site in this fixture.
func CallGreeter() string {
	g := NewGreeter()
	return g.Greet()
}
`
	locs.appFile = writeE2EFile(t, root, "app/app.go", appSrc)

	locs.concreteMethodDeclPos = mustPos(t, appSrc, "func (h Human) Greet() string {", "Greet")
	locs.interfaceCallPos = mustPos(t, appSrc, "return g.Greet()", "Greet")

	return root, locs
}

// TestE2E_ReferencesOnConcreteMethodFindsInterfaceTypedCallSite drives a
// real golance binary over stdio, covering textDocument/references
// (internal/server/handlers_xref.go's handleReferences, backed by
// internal/xref.Resolver.References) invoked on a CONCRETE method's own
// declaration in a dependency-injection-shaped fixture: the only call site
// in the workspace goes through the interface-typed value a constructor
// returns, never a concretely-typed one. Before
// internal/xref.interfacesSatisfiedByMethod, this returned no references at
// all beyond the declaration itself.
func TestE2E_ReferencesOnConcreteMethodFindsInterfaceTypedCallSite(t *testing.T) {
	skipUnlessE2E(t)

	root, locs := writeE2ERefsReverseModule(t)
	c := startClient(t, root)
	c.initialize(t, root)

	c.openFile(t, locs.appFile)
	c.waitForIndexReady(t)

	got := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentReferences, &protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.appFile)},
			Position:     locs.concreteMethodDeclPos,
		},
		Context: protocol.ReferenceContext{IncludeDeclaration: false},
	}, e2eRequestBudget)

	found := false
	for _, l := range got {
		if l.URI.FsPath() == locs.appFile && l.Range.Start.Line == locs.interfaceCallPos.Line {
			found = true
		}
	}
	if !found {
		t.Errorf("references(Human.Greet) = %+v, missing the interface-typed call site at %s:%d", got, locs.appFile, locs.interfaceCallPos.Line)
	}
}
