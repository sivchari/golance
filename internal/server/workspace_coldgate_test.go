package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/depcheck"
	"github.com/sivchari/golance/internal/depexport"
	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/typecheck"
)

// buildColdGateDepCache returns a depCacheHolder wired exactly like
// production setWorkspace (a real depexport.Cache over a real
// depcheck.Provider — see ensureDepProvider's own doc), resolving imports
// against depcheck's own "example.com/depcheckmod" fixture module, plus
// that Provider itself so a test can assert whether a from-source check
// actually ran. indexReady lets each test pick which importer() gate path
// (internal/server/workspace.go) is exercised.
func buildColdGateDepCache(t *testing.T, indexReady func() bool) (holder *depCacheHolder, exportProvider *depcheck.Provider) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "depcheck", "testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	meta := depcheck.NewGraphMetadataSource(snap)
	exportProvider = depcheck.NewProvider(meta, depcheck.Options{})
	cas, err := store.OpenCAS(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	cache := depexport.NewCache(cas, meta, exportProvider, depexport.Options{})
	return newDepCacheHolder(cache, nil, nil, indexReady), exportProvider
}

// depImportSrc is a single-file package importing depcheck's own
// "example.com/depcheckmod/dep" fixture and reading a .Msg field off its
// instantiated generic Box type — the exact connect-style shape the
// approved fix direction requires warm cross-package resolution to keep
// answering exactly as before (see this package's own cold-index-build gate
// doc). Local has no dependency on that import at all, so it must still
// resolve correctly even on a run where the import itself could not be.
const depImportSrc = `package root

import "example.com/depcheckmod/dep"

type payload struct{ Name string }

// Consume mirrors a connect-style .Msg field access on an instantiated
// generic dependency type.
func Consume() string {
	b := dep.Box[payload]{Msg: &payload{Name: "x"}}
	return b.Msg.Name
}

// Local has no dependency on the import above.
func Local() int { return 1 }
`

// checkDepImportSrc type-checks depImportSrc through imp, exactly the way
// check.Engine.runRecheck resolves a recheck's dependencies via
// typecheck.CheckPackage.
func checkDepImportSrc(t *testing.T, imp types.ImporterFrom) (*types.Package, *types.Info, []types.Error) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "root.go", depImportSrc, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse depImportSrc: %v", err)
	}
	return typecheck.CheckPackage(fset, []*ast.File{f}, "example.com/coldgate/root", imp)
}

// TestDepCacheHolder_Importer_ColdIndexBuildDoesNotSourceCheckClosure is the
// regression test for the cold-start server RSS blowup: while the facts
// index is not ready (indexReady returns false — a cold index build still
// running), depCacheHolder.importer must resolve an uncached cross-package
// import WITHOUT ever falling through to depexport.Cache's recursive
// from-source check — the exact chain (check.Engine.runRecheck ->
// typecheck.CheckPackage -> ... -> depexport.Cache.checkAndPersist) that
// duplicated, in the server process, work the indexer subprocess was
// already doing. exportProvider.Checked() staying 0 proves no from-source
// check of "example.com/depcheckmod/dep" (or its own transitive imports)
// ever ran.
func TestDepCacheHolder_Importer_ColdIndexBuildDoesNotSourceCheckClosure(t *testing.T) {
	holder, exportProvider := buildColdGateDepCache(t, func() bool { return false })

	pkg, info, errs := checkDepImportSrc(t, holder.importer())

	if got := exportProvider.Checked(); got != 0 {
		t.Errorf("exportProvider.Checked() = %d, want 0 (a cold index build must never source-check the closure)", got)
	}
	if pkg == nil {
		t.Fatal("CheckPackage returned a nil *types.Package")
	}
	if len(errs) == 0 {
		t.Error("CheckPackage returned no errors for an unresolved import during a cold build, want at least one")
	}

	local := pkg.Scope().Lookup("Local")
	if local == nil {
		t.Fatal(`pkg.Scope().Lookup("Local") = nil, want the local declaration to resolve despite the unresolved import`)
	}
	sig, ok := local.Type().(*types.Signature)
	if !ok {
		t.Fatalf("Local's resolved type = %T, want *types.Signature", local.Type())
	}
	basic, ok := sig.Results().At(0).Type().(*types.Basic)
	if !ok {
		t.Fatalf("Local's resolved return type = %T, want *types.Basic", sig.Results().At(0).Type())
	}
	if got := basic.Kind(); got != types.Int {
		t.Errorf("Local's resolved return type = %v, want int", got)
	}
	if _, ok := info.Defs[localFuncIdent(t, info)]; !ok {
		t.Error("info.Defs has no entry for Local's own identifier")
	}
}

// localFuncIdent returns the *ast.Ident info.Defs recorded for the "Local"
// function declaration, for TestDepCacheHolder_Importer_
// ColdIndexBuildDoesNotSourceCheckClosure's own Defs assertion.
func localFuncIdent(t *testing.T, info *types.Info) *ast.Ident {
	t.Helper()
	for id, obj := range info.Defs {
		if obj != nil && obj.Name() == "Local" {
			return id
		}
	}
	t.Fatal(`no info.Defs entry has an object named "Local"`)
	return nil
}

// TestDepCacheHolder_Importer_WarmIndexResolvesCrossPackageMsgField is the
// guardrail half of the cold-index-build gate: once the facts index is
// ready (indexReady returns true), depCacheHolder.importer must resolve a
// cross-package import exactly as before the gate existed — including a
// connect-style .Msg field on an instantiated generic dependency type,
// which only type-checks without error if Box[payload]'s Msg field really
// resolved to *payload. exportProvider.Checked() > 0 confirms the
// from-source check this depends on actually ran.
func TestDepCacheHolder_Importer_WarmIndexResolvesCrossPackageMsgField(t *testing.T) {
	holder, exportProvider := buildColdGateDepCache(t, func() bool { return true })

	pkg, _, errs := checkDepImportSrc(t, holder.importer())

	if got := exportProvider.Checked(); got == 0 {
		t.Error("exportProvider.Checked() = 0, want > 0 (warm resolution must still source-check the dependency)")
	}
	for _, e := range errs {
		t.Errorf("unexpected type error: %v", e)
	}
	if pkg == nil || !pkg.Complete() {
		t.Fatal("warm CheckPackage did not produce a complete *types.Package")
	}
}

// TestDepCacheHolder_Importer_ColdMissCarriesMarker pins the discrimination
// mechanism publishDiagnostics (internal/server/diagnostics.go) relies on
// for B1: while the facts index is not ready, an import
// depCacheHolder.importer cannot yet resolve must produce an error whose
// text contains coldGateMissMarker -- the one signal that survives
// go/types' own "could not import %s (%s)" folding down to a plain Msg
// string (see isColdGateImportDiag's doc in diagnostics.go for why that
// folding rules out anything sturdier).
func TestDepCacheHolder_Importer_ColdMissCarriesMarker(t *testing.T) {
	holder, _ := buildColdGateDepCache(t, func() bool { return false })

	_, _, errs := checkDepImportSrc(t, holder.importer())

	if len(errs) == 0 {
		t.Fatal("checkDepImportSrc returned no errors, want at least one for the unresolved import")
	}
	for _, e := range errs {
		if strings.Contains(e.Msg, coldGateMissMarker) {
			return
		}
	}
	var msgs []string
	for _, e := range errs {
		msgs = append(msgs, e.Msg)
	}
	t.Errorf("no error message contains coldGateMissMarker (%q): %v", coldGateMissMarker, msgs)
}
