package golance_test

import (
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// e2eEmbedLocs records the exact 0-based positions
// TestE2E_ImplementationAndReferencesThroughEmbedding queries, captured
// while the synthetic module is written so nothing is re-parsed at query
// time.
type e2eEmbedLocs struct {
	greetFile string // lib/greet/greet.go
	baseFile  string // lib/base/base.go
	appFile   string // app/app.go

	interfaceMethodPos protocol.Position // "Greet" inside the Greeter interface body
	baseMethodDeclPos  protocol.Position // "Greet" at Base's own method declaration
	interfaceCallPos   protocol.Position // "Greet" in CallInterface's g.Greet()
	concreteCallPos    protocol.Position // "Greet" in CallConcrete's Human{}.Greet()
}

func writeE2EEmbedModule(t *testing.T) (string, e2eEmbedLocs) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	var locs e2eEmbedLocs
	writeE2EFile(t, root, "go.mod", "module example.com/e2eembed\n\ngo 1.23\n")

	const greetSrc = `package greet

// Greeter can greet and label itself. Two methods (rather than one) keep
// base.Base below from independently satisfying Greeter on its own -- it
// only ever contributes Greet, never Label -- so Human is Greeter's one and
// only implementer here.
type Greeter interface {
	Greet() string
	Label() string
}
`
	locs.greetFile = writeE2EFile(t, root, "lib/greet/greet.go", greetSrc)

	const baseSrc = `package base

// Base declares Greet; Human implements greet.Greeter's Greet only by
// embedding it (Label is Human's own, declared directly in app.go).
type Base struct{}

func (b Base) Greet() string {
	return "hi from base"
}
`
	locs.baseFile = writeE2EFile(t, root, "lib/base/base.go", baseSrc)

	const appSrc = `package app

import (
	"example.com/e2eembed/lib/base"
	"example.com/e2eembed/lib/greet"
)

// Human implements Greeter: Label directly, Greet only via embedding
// base.Base.
type Human struct {
	base.Base
}

// Label returns Human's own label.
func (h Human) Label() string {
	return "human"
}

// CallInterface calls Greet through an interface-typed parameter.
func CallInterface(g greet.Greeter) string {
	return g.Greet()
}

// CallConcrete calls Greet through a concretely-typed value.
func CallConcrete() string {
	return Human{}.Greet()
}
`
	locs.appFile = writeE2EFile(t, root, "app/app.go", appSrc)

	locs.interfaceMethodPos = mustPos(t, greetSrc, "Greet() string", "Greet")
	locs.baseMethodDeclPos = mustPos(t, baseSrc, "func (b Base) Greet() string {", "Greet")
	locs.interfaceCallPos = mustPos(t, appSrc, "return g.Greet()", "Greet")
	locs.concreteCallPos = mustPos(t, appSrc, "return Human{}.Greet()", "Greet")

	return root, locs
}

// TestE2E_ImplementationAndReferencesThroughEmbedding drives a real golance
// binary over stdio against an implementer (Human) that only satisfies an
// interface (Greeter) by embedding a helper type (base.Base) providing the
// required method. It covers both halves of the fix this test file
// accompanies:
//
//   - textDocument/implementation on the interface method itself
//     (internal/xref/implementation.go's methodImplementations) must resolve
//     to base.Base's own Greet declaration, since Human has no Greet of its
//     own to point at.
//   - textDocument/references on the interface method
//     (internal/xref/xref.go's References) must include the call site
//     through Human, a concretely-typed value, even though that call is
//     recorded (by info.Selections) against base.Base's Greet, not
//     Greeter's.
func TestE2E_ImplementationAndReferencesThroughEmbedding(t *testing.T) {
	skipUnlessE2E(t)

	root, locs := writeE2EEmbedModule(t)
	c := startClient(t, root)
	c.initialize(t, root)

	c.openFile(t, locs.appFile)
	c.waitForIndexReady(t)

	t.Run("implementation_on_interface_method_finds_embedded_declaration", func(t *testing.T) {
		result := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentImplementation, &protocol.ImplementationParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.greetFile)},
				Position:     locs.interfaceMethodPos,
			},
		}, e2eIndexBudget)
		if len(result) != 1 {
			t.Fatalf("implementation returned %d locations, want 1 (base.Base.Greet): %+v", len(result), result)
		}
		if gotPath := result[0].URI.FsPath(); gotPath != locs.baseFile || result[0].Range.Start.Line != locs.baseMethodDeclPos.Line {
			t.Errorf("implementation = %s:%d, want %s:%d", gotPath, result[0].Range.Start.Line, locs.baseFile, locs.baseMethodDeclPos.Line)
		}
	})

	t.Run("references_on_interface_method_includes_embedded_concrete_call_site", func(t *testing.T) {
		got := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentReferences, &protocol.ReferenceParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.greetFile)},
				Position:     locs.interfaceMethodPos,
			},
			Context: protocol.ReferenceContext{IncludeDeclaration: false},
		}, e2eRequestBudget)

		var foundInterfaceCall, foundConcreteCall bool
		for _, l := range got {
			if l.URI.FsPath() != locs.appFile {
				continue
			}
			switch l.Range.Start.Line {
			case locs.interfaceCallPos.Line:
				foundInterfaceCall = true
			case locs.concreteCallPos.Line:
				foundConcreteCall = true
			}
		}
		if !foundInterfaceCall {
			t.Errorf("references on Greeter.Greet missing the interface-typed call site; got %d location(s): %+v", len(got), got)
		}
		if !foundConcreteCall {
			t.Errorf("references on Greeter.Greet missing the concrete-typed call site through the embedded method; got %d location(s): %+v", len(got), got)
		}
	})
}
