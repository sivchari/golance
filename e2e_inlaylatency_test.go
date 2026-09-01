package golance_test

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// initializeWithInlayRefresh performs the LSP handshake declaring
// workspace.inlayHint.refreshSupport, the capability
// clientSupportsInlayHintRefresh gates workspace/inlayHint/refresh pushes on
// (see internal/server/lifecycle.go) — none of the other e2e_*_test.go
// clients declare it, so timeUntilServerRequest below would otherwise never
// see one.
func initializeWithInlayRefresh(t *testing.T, c *lspClient, root string) {
	t.Helper()
	gopid := os.Getpid()
	if gopid < 0 || gopid > math.MaxInt32 {
		t.Fatalf("pid %d does not fit in int32", gopid)
		return
	}
	pid := int32(gopid)
	trueVal := true
	params := &protocol.InitializeParams{
		ProcessID: &pid,
		Capabilities: protocol.ClientCapabilities{
			Workspace: &protocol.WorkspaceClientCapabilities{
				InlayHint: &protocol.InlayHintWorkspaceClientCapabilities{RefreshSupport: &trueVal},
			},
		},
		WorkspaceFoldersInitializeParams: protocol.WorkspaceFoldersInitializeParams{
			WorkspaceFolders: protocol.NewNullable([]protocol.WorkspaceFolder{
				{URI: uri.File(root), Name: filepath.Base(root)},
			}),
		},
	}
	resp := c.call(t, protocol.MethodInitialize, params, e2eIndexBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("initialize failed: %s", resp.Error)
	}
	c.notify(t, protocol.MethodInitialized, &protocol.InitializedParams{})
}

// TestE2EInlayHintLatencyProfile is a measurement harness, not a
// correctness test: it drives a real golance binary over stdio against a
// realistically-sized generated file and reports (via t.Log) the
// server-side response latency of textDocument/inlayHint against
// textDocument/hover and textDocument/semanticTokens/full, cold (first
// request against the package), warm (cache hit), and immediately after a
// didChange (forces a recheck). It also measures the didChange -> recheck
// -> publishDiagnostics -> workspace/inlayHint/refresh round trip.
//
// Every package this test queries (bigFile plus the four cold-comparison
// packages) is written to disk before the client ever calls "initialize":
// golance resolves textDocument/* requests against the import graph
// snapshot loaded at initialize time (internal/check.GraphSource.
// PackageForFile), so a package written afterward — without a
// workspace/didChangeWatchedFiles round trip this minimal client never
// registers for — would be invisible to it and every request against it
// would fail fast instead of exercising a real type-check.
//
// It is gated by skipUnlessE2E like the rest of the GOLANCE_E2E suite; run
// it in isolation with:
//
//	GOLANCE_E2E=1 go test -run TestE2EInlayHintLatencyProfile -v .
func TestE2EInlayHintLatencyProfile(t *testing.T) {
	skipUnlessE2E(t)

	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	writeE2EFile(t, root, "go.mod", "module example.com/latency\n\ngo 1.23\n")

	// bigSrc is a single ~2500-line file with a high density of every hint
	// kind InlayHints produces (assignVariableTypes, parameterNames,
	// rangeVariableTypes, compositeLiteralFields/Types, constantValues,
	// functionTypeParameters), patterned like generated/DTO-heavy files
	// gopls users report slow inlay hints on.
	bigSrc := genLatencySource(latencyNumBlocks)
	bigFile := writeE2EFile(t, root, "big/big.go", bigSrc)
	lines := strings.Split(bigSrc, "\n")
	t.Logf("generated file: %d lines, %d bytes, %d blocks", len(lines), len(bigSrc), latencyNumBlocks)

	// Every package the "cold" subtests below query, written up front (see
	// the test's own doc for why).
	coldHoverFile, coldHoverSrc := newLatencyPackageSource(root, "coldhover")
	coldInlayFullFile, coldInlayFullSrc := newLatencyPackageSource(root, "coldinlayfull")
	coldInlayVPFile, coldInlayVPSrc := newLatencyPackageSource(root, "coldinlayvp")
	coldSemFile, _ := newLatencyPackageSource(root, "coldsem")

	c := startClient(t, root)
	initializeWithInlayRefresh(t, c, root)

	c.openFile(t, bigFile)
	// The initial didOpen kicks off its own debounced recheck via
	// Invalidate; drain the diagnostics it publishes so it cannot race the
	// timed requests below (a concurrent background recheck would otherwise
	// sometimes win the race to populate Engine's cache first, silently
	// turning a "cold" measurement into a warm one).
	c.waitForDiagnostics(t, bigFile)

	fullRange := protocol.Range{End: endOfDocument(bigSrc)}
	// viewportRange mimics what an editor actually sends: the visible
	// portion of the file around the middle, not the whole document.
	midLine := lineU32(t, len(lines)/2)
	viewportRange := protocol.Range{
		Start: protocol.Position{Line: midLine, Character: 0},
		End:   protocol.Position{Line: midLine + 40, Character: 0},
	}
	midPos := protocol.Position{Line: midLine, Character: 1}

	t.Run("cold", func(t *testing.T) {
		// Each subcase opens its own never-yet-queried package (same shape
		// as bigSrc) and immediately times the first request against it —
		// nothing in check.Engine's cache yet, forcing a synchronous
		// type-check inside the request itself.
		t.Run("hover", func(t *testing.T) {
			c.openFile(t, coldHoverFile)
			d := timeRequest(t, c, protocol.MethodTextDocumentHover, &protocol.HoverParams{
				TextDocumentPositionParams: protocol.TextDocumentPositionParams{
					TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(coldHoverFile)},
					Position:     midPosition(t, coldHoverSrc),
				},
			})
			t.Logf("cold hover: %s", d)
		})
		t.Run("inlayHint_full", func(t *testing.T) {
			c.openFile(t, coldInlayFullFile)
			d := timeRequest(t, c, protocol.MethodTextDocumentInlayHint, &protocol.InlayHintParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(coldInlayFullFile)},
				Range:        protocol.Range{End: endOfDocument(coldInlayFullSrc)},
			})
			t.Logf("cold inlayHint (full-document range): %s", d)
		})
		t.Run("inlayHint_viewport", func(t *testing.T) {
			c.openFile(t, coldInlayVPFile)
			srcLines := strings.Split(coldInlayVPSrc, "\n")
			rng := protocol.Range{
				Start: protocol.Position{Line: lineU32(t, len(srcLines)/2), Character: 0},
				End:   protocol.Position{Line: lineU32(t, len(srcLines)/2+40), Character: 0},
			}
			d := timeRequest(t, c, protocol.MethodTextDocumentInlayHint, &protocol.InlayHintParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(coldInlayVPFile)},
				Range:        rng,
			})
			t.Logf("cold inlayHint (viewport range): %s", d)
		})
		t.Run("semanticTokensFull", func(t *testing.T) {
			c.openFile(t, coldSemFile)
			d := timeRequest(t, c, protocol.MethodTextDocumentSemanticTokensFull, &protocol.SemanticTokensParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(coldSemFile)},
			})
			t.Logf("cold semanticTokens/full: %s", d)
		})
	})

	t.Run("warm", func(t *testing.T) {
		// bigFile's package is already checked and cached (see the
		// diagnostics drain above), so every request below is a cache hit:
		// this isolates each method's own per-request compute cost from
		// type-checking cost.
		dHover := timeRequest(t, c, protocol.MethodTextDocumentHover, &protocol.HoverParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(bigFile)},
				Position:     midPos,
			},
		})
		t.Logf("warm hover: %s", dHover)

		dInlayFull := timeRequest(t, c, protocol.MethodTextDocumentInlayHint, &protocol.InlayHintParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(bigFile)},
			Range:        fullRange,
		})
		t.Logf("warm inlayHint (full-document range, %d lines): %s", len(lines), dInlayFull)

		dInlayViewport := timeRequest(t, c, protocol.MethodTextDocumentInlayHint, &protocol.InlayHintParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(bigFile)},
			Range:        viewportRange,
		})
		t.Logf("warm inlayHint (viewport range, 40 of %d lines): %s", len(lines), dInlayViewport)

		dSem := timeRequest(t, c, protocol.MethodTextDocumentSemanticTokensFull, &protocol.SemanticTokensParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(bigFile)},
		})
		t.Logf("warm semanticTokens/full: %s", dSem)
	})

	t.Run("post_edit", func(t *testing.T) {
		// Append a trivial no-op comment line so the content hash changes
		// (forcing Engine.Get to recheck) without changing anything the
		// hints/hover/tokens under test depend on.
		edited := bigSrc + "\n// edit marker\n"
		version := int32(2)

		c.changeFile(t, bigFile, version, edited)
		dInlayViewport := timeRequest(t, c, protocol.MethodTextDocumentInlayHint, &protocol.InlayHintParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(bigFile)},
			Range:        viewportRange,
		})
		t.Logf("post-edit inlayHint (viewport range): %s", dInlayViewport)
		c.waitForDiagnostics(t, bigFile) // drain before the next edit below

		version++
		c.changeFile(t, bigFile, version, edited+"\n// edit marker 2\n")
		dHover := timeRequest(t, c, protocol.MethodTextDocumentHover, &protocol.HoverParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(bigFile)},
				Position:     midPos,
			},
		})
		t.Logf("post-edit hover: %s", dHover)
		c.waitForDiagnostics(t, bigFile)
	})

	t.Run("refresh_round_trip", func(t *testing.T) {
		// Every directory recheck triggered by an earlier subtest's own
		// didOpen/didChange (cold's four package opens, warm's none,
		// post_edit's two edits) eventually fires its own
		// workspace/inlayHint/refresh in the background — workspace/
		// inlayHint/refresh carries no params, so the client cannot tell
		// those apart from the one this subtest's own edit triggers.
		// Drain whatever is already queued so timeUntilServerRequest below
		// cannot pick up a stale one (surfaces as a large negative
		// "didChange -> refresh" duration).
		drainServerRequests(c)

		editStart := time.Now()
		version := int32(10)
		edited := bigSrc + fmt.Sprintf("\n// refresh marker %d\n", version)
		c.changeFile(t, bigFile, version, edited)

		diagAt := timeUntilDiagnostics(t, c, bigFile)
		t.Logf("didChange -> publishDiagnostics: %s", diagAt.Sub(editStart))

		refreshAt := timeUntilServerRequest(t, c, protocol.MethodWorkspaceInlayHintRefresh)
		t.Logf("didChange -> workspace/inlayHint/refresh: %s", refreshAt.Sub(editStart))
		t.Logf("publishDiagnostics -> workspace/inlayHint/refresh: %s", refreshAt.Sub(diagAt))
	})
}

// timeRequest sends method/params to c and returns how long the response
// took to arrive.
func timeRequest(t *testing.T, c *lspClient, method string, params any) time.Duration {
	t.Helper()
	start := time.Now()
	resp := c.call(t, method, params, e2eRequestBudget)
	elapsed := time.Since(start)
	if len(resp.Error) > 0 {
		t.Fatalf("%s failed: %s", method, resp.Error)
	}
	return elapsed
}

// timeUntilDiagnostics blocks until a publishDiagnostics notification for
// path arrives and returns the time it arrived at.
func timeUntilDiagnostics(t *testing.T, c *lspClient, path string) time.Time {
	t.Helper()
	want := string(uri.File(path))
	deadline := time.After(e2eRequestBudget)
	for {
		select {
		case n := <-c.diagnostics:
			if n.uri == want {
				return time.Now()
			}
		case <-deadline:
			t.Fatalf("no publishDiagnostics for %s within %s", path, e2eRequestBudget)
			return time.Time{}
		}
	}
}

// timeUntilServerRequest blocks until a server-initiated request for method
// arrives and returns the time it arrived at.
func timeUntilServerRequest(t *testing.T, c *lspClient, method string) time.Time {
	t.Helper()
	deadline := time.After(e2eRequestBudget)
	for {
		select {
		case r := <-c.requests:
			if r.method == method {
				return r.receivedAt
			}
		case <-deadline:
			t.Fatalf("no server-initiated %s within %s", method, e2eRequestBudget)
			return time.Time{}
		}
	}
}

// drainServerRequests discards every serverRequest already queued on c,
// without blocking once the channel is empty.
func drainServerRequests(c *lspClient) {
	for {
		select {
		case <-c.requests:
		default:
			return
		}
	}
}

// midPosition returns a position roughly in the middle of src, at the start
// of a line, good enough for a hover request to land on real code without
// needing to know src's exact shape.
func midPosition(t *testing.T, src string) protocol.Position {
	t.Helper()
	lines := strings.Split(src, "\n")
	return protocol.Position{Line: lineU32(t, len(lines)/2), Character: 1}
}

// lineU32 converts a line index to the uint32 LSP wire type, failing the
// test on a value outside the protocol's range.
func lineU32(t *testing.T, n int) uint32 {
	t.Helper()
	if n < 0 || int64(n) > math.MaxUint32 {
		t.Fatalf("line index out of uint32 range: %d", n)
	}
	return uint32(n)
}

// newLatencyPackageSource writes a fresh package named pkgName under root
// (on disk only — it does not open a client connection), with the same
// genLatencySource shape bigSrc uses, so a "cold" measurement against it
// exercises a full type-check of equivalent size and shape without reusing
// bigFile's already-warmed check.Engine cache entry. Callers must write
// every such package before starting the client (see
// TestE2EInlayHintLatencyProfile's own doc for why).
func newLatencyPackageSource(root, pkgName string) (path, src string) {
	src = genLatencySource(latencyNumBlocks)
	src = strings.Replace(src, "package biglatency", "package "+pkgName, 1)
	path = filepath.Join(root, filepath.FromSlash(pkgName), pkgName+".go")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		panic(err) // t is unavailable here; writeE2EFile-shaped callers all run before the client starts, so this only fails on a broken tmp dir.
	}
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		panic(err)
	}
	return path, src
}

// genLatencySource generates a single Go source file with numBlocks
// repetitions of a block exercising every langfeat.HintKind InlayHints
// produces: assignVariableTypes (":="), parameterNames (named-parameter
// calls), rangeVariableTypes ("for k, v := range"), compositeLiteralFields
// and compositeLiteralTypes (unkeyed struct literals, elided element
// types), constantValues (an iota block), and functionTypeParameters (a
// generic call with inferred type arguments) — patterned after the
// generated/DTO-heavy files gopls users report slow inlay hints on.
// latencyNumBlocks is the shared size of every generated latency-test
// package, so cold packages match bigSrc's shape exactly.
const latencyNumBlocks = 400

func genLatencySource(numBlocks int) string {
	var b strings.Builder
	b.WriteString(`package biglatency

type Point struct {
	X, Y int
}

func NewPoint(x, y int) Point { return Point{X: x, Y: y} }

func Sum[T int | float64](a, b T) T { return a + b }

const (
	KindA = iota
	KindB
	KindC
)

`)
	for i := 0; i < numBlocks; i++ {
		fmt.Fprintf(&b, `// Block%d exercises every inlay hint kind.
func Block%d() int {
	total := NewPoint(%d, %d)
	unkeyed := Point{%d, %d}
	pts := []Point{{X: %d, Y: %d}, {X: %d, Y: %d}}
	for i, p := range pts {
		_ = i
		total = NewPoint(total.X+p.X, total.Y+p.Y)
	}
	sum := Sum(total.X, total.Y)
	return sum + unkeyed.X
}

`, i, i, i, i+1, i, i+1, i, i+1, i+2, i+3)
	}
	return b.String()
}
