package depexport

import (
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/depcheck"
	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/typecheck"
)

// generateThrashModule writes a module whose single root package imports
// net/http — a stdlib package whose own transitive closure is large enough
// (~170 distinct packages) to thrash a small depcheck.Provider LRU (see
// depcheck.DefaultCap's own doc on why a cap smaller than a single check's
// own closure thrashes it). Confirmed empirically: at a small cap, the same
// import path can get evicted and re-checked from scratch partway through
// one recursive check, so cp.Types() ends up referencing two
// non-identical *types.Package instances for the same import path.
func generateThrashModule(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/thrashmod\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	src := "package root\n\nimport \"net/http\"\n\nfunc F() http.Handler { return nil }\n"
	if err := os.WriteFile(filepath.Join(dir, "root.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write root.go: %v", err)
	}
}

// TestCache_UndersizedCapNeverReturnsAnUndecodableBlob pins the invariant
// documented on Cache and checkAndPersist: ExportData must never report
// ok=true for a blob that gcexportdata.Read cannot decode back, even when
// depcheck.Provider's own LRU is too small for the closure being checked
// and would otherwise silently serialize an internally-inconsistent
// *types.Package graph (see checkAndPersist's own doc for the mechanism —
// confirmed reproducible with the decode fast path both on and off, so
// this is independent of depcheck.Provider.SetExportSource). Before
// checkAndPersist's own round-trip self-check, this exact scenario
// returned ok=true with corrupt bytes that gcexportdata.Read panicked
// decoding elsewhere (in a live session, inside check.Engine's own
// dependency importer; in a batch build, inside a worker sharing the
// decoded result — see internal/server.ensureDepProvider's doc for the
// three symptoms this closes). Now it must surface as an ordinary,
// contained error instead.
func TestCache_UndersizedCapNeverReturnsAnUndecodableBlob(t *testing.T) {
	dir := t.TempDir()
	generateThrashModule(t, dir)
	snap, err := graph.Load(graph.Options{Dir: dir}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	meta := depcheck.NewGraphMetadataSource(snap)
	// depcheck.DefaultCap (64, Options{} zero value): smaller than
	// net/http's own transitive closure (~170 distinct packages) — forces
	// the exact LRU thrash this test exists to catch. A far smaller cap
	// (single digits) was tried and rejected: it thrashes so badly the
	// recursive re-check blows up past any reasonable test timeout, not
	// just past the closure size.
	provider := depcheck.NewProvider(meta, depcheck.Options{})
	cache := NewCache(nil, meta, provider, Options{})

	const target = "example.com/thrashmod"
	blob, ok, err := cache.ExportData(target)
	if err != nil {
		t.Logf("ExportData correctly refused an undecodable blob instead of returning corrupt bytes: %v", err)
		if !strings.Contains(err.Error(), "declaration-only check reported") {
			t.Errorf("error message is missing the declaration-only check's own first-error diagnostic: %v", err)
		}
		return
	}
	if !ok {
		t.Fatal("ExportData: ok = false with no error")
	}
	if _, decodeErr := typecheck.ReadExport(blob, token.NewFileSet(), target, typecheck.NewCache()); decodeErr != nil {
		t.Fatalf("ExportData returned ok=true with a blob that fails to round-trip decode: %v", decodeErr)
	}
}

// TestCache_GenericWrapperFieldRoundTripsThroughWorkspaceImporter checks
// the common, well-provisioned case end to end: a field reached through an
// instantiated generic dependency type — box.Box[payload.Data].Msg,
// mirroring connectrpc.com/connect's Request[T].Msg, the shape whose
// resolution silently broke under the pre-fix corruption (see
// TestCache_UndersizedCapNeverReturnsAnUndecodableBlob) — resolves to its
// real, non-Invalid type when a workspace-style consumer package is
// checked through typecheck.Importer with Cache wired in as its
// ExportSource, exactly like internal/server's depCacheHolder wires
// depexport.Cache in for compiling a workspace package. Red-then-green
// against the pre-round-trip-check tree: reverting checkAndPersist's
// self-check does not change this specific fixture's outcome (its closure
// is far too small to thrash — that is deliberate, see this test's own
// doc), so this test instead pins the invariant that the common case keeps
// working now that the safety net exists.
func TestCache_GenericWrapperFieldRoundTripsThroughWorkspaceImporter(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "genericclosure"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./consumer/...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	meta := depcheck.NewGraphMetadataSource(snap)
	provider := depcheck.NewProvider(meta, depcheck.Options{})
	cache := NewCache(nil, meta, provider, Options{})

	const target = "example.com/genericclosure/consumer"
	blob, ok, err := cache.ExportData(target)
	if err != nil {
		t.Fatalf("ExportData(%s): %v", target, err)
	}
	if !ok {
		t.Fatalf("ExportData(%s): ok=false", target)
	}
	pkg, err := typecheck.ReadExport(blob, token.NewFileSet(), target, typecheck.NewCache())
	if err != nil {
		t.Fatalf("ReadExport(%s): %v", target, err)
	}
	obj := pkg.Scope().Lookup("F")
	if obj == nil {
		t.Fatal("F not found in decoded consumer package")
	}
	sig, ok := obj.Type().(*types.Signature)
	if !ok || sig.Params().Len() != 1 {
		t.Fatalf("F type = %v, want a 1-parameter *types.Signature", obj.Type())
	}
	paramType := sig.Params().At(0).Type()
	if paramType == types.Typ[types.Invalid] {
		t.Fatal("F's box.Box[payload.Data] parameter resolved to types.Invalid")
	}
	if paramType.String() != "*example.com/genericclosure/box.Box[example.com/genericclosure/payload.Data]" {
		t.Fatalf("F param type = %v, want *box.Box[payload.Data]", paramType)
	}
}
