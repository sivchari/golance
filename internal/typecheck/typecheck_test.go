// Package typecheck_test is an external test package, not an internal one,
// specifically so it can import internal/depcheck (for a real,
// declaration-only ExportSource fixture — see stdlibExportSource) without an
// import cycle: internal/depcheck itself imports internal/typecheck (for its
// own transitive-import decode fast path, see depcheck.ExportSource's doc),
// and an internal (same-package) test file cannot import anything that
// imports the package under test, only an external one like this can. Every
// symbol this file exercises is already part of typecheck's public API, so
// nothing here needed package-private access in the first place.
package typecheck_test

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/depcheck"
	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/typecheck"
)

// stdlibExportSource resolves stdlib packages the same way production now
// does (see internal/depexport's package doc): declaration-only
// source-type-checking them via internal/depcheck, never a GOCACHE-generated
// `go list -export` file. Kept as its own tiny ExportSource here, rather
// than depending on internal/depexport directly, so this package's own
// tests stay focused on Importer/Cache's two-tier resolution logic, not on
// depexport's separate persistence behavior (covered by its own tests).
type stdlibExportSource struct {
	provider *depcheck.Provider
}

// newStdlibExportSource loads this test module's own real *graph.Snapshot
// (testdata/module — a genuine go.mod, so graph.Load resolves real stdlib
// packages exactly as production does) and wraps a fresh depcheck.Provider
// over it.
func newStdlibExportSource(t *testing.T) stdlibExportSource {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	meta := depcheck.NewGraphMetadataSource(snap)
	return stdlibExportSource{provider: depcheck.NewProvider(meta, depcheck.Options{})}
}

// ExportData implements ExportSource by declaration-only source-checking
// pkgPath via s.provider and re-encoding the result as a self-contained
// blob (WriteExport) — the same shape a real fallback ExportSource
// (internal/depexport.Cache) produces. A check failure propagates as an
// error rather than a soft ok=false miss: unlike "pkgPath is not part of
// this module's import graph at all" (the genuine soft-miss case every
// other ExportSource in this file models), a failed check for a pkgPath
// the test itself asked for is a real, unexpected failure that should fail
// the test loudly instead of silently falling through to "no export data".
func (s stdlibExportSource) ExportData(pkgPath string) ([]byte, bool, error) {
	cp, err := s.provider.Package(context.Background(), pkgPath)
	if err != nil {
		return nil, false, err
	}
	blob, err := typecheck.WriteExport(cp.Types(), s.provider.FileSet())
	if err != nil {
		return nil, false, err
	}
	return blob, true, nil
}

type blobSource struct {
	blobs map[string][]byte
}

func (s blobSource) ExportData(pkgPath string) ([]byte, bool, error) {
	b, ok := s.blobs[pkgPath]
	return b, ok, nil
}

func parseTestdata(t *testing.T, fset *token.FileSet, rel string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(fset, filepath.Join("testdata", "module", rel), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	return f
}

// TestCheckPackage_StdlibExportFile checks a leaf package whose only import
// is stdlib "fmt", resolved via the fallback ExportSource tier
// (stdlibExportSource).
func TestCheckPackage_StdlibExportFile(t *testing.T) {
	fset := token.NewFileSet()
	f := parseTestdata(t, fset, "dep/dep.go")

	cache := typecheck.NewCache()
	imp := typecheck.NewImporter(fset, nil, newStdlibExportSource(t), cache)

	pkg, info, errs := typecheck.CheckPackage(fset, []*ast.File{f}, "example.com/tcmod/dep", imp)
	if len(errs) != 0 {
		t.Fatalf("unexpected type errors: %v", errs)
	}
	if pkg == nil || !pkg.Complete() {
		t.Fatalf("expected a complete package, got %v", pkg)
	}
	if len(info.Defs) == 0 {
		t.Error("info.Defs is empty, want Greet's declaration recorded")
	}
}

// TestCheckPackage_ExportSourceDependency checks a package whose workspace
// dependency is resolved from a self-authored WriteExport blob via the
// primary ExportSource, and whose stdlib dependency still resolves via the
// fallback tier (stdlibExportSource).
func TestCheckPackage_ExportSourceDependency(t *testing.T) {
	fset := token.NewFileSet()

	depFile := parseTestdata(t, fset, "dep/dep.go")
	cache := typecheck.NewCache()
	depImp := typecheck.NewImporter(fset, nil, newStdlibExportSource(t), cache)
	depPkg, _, errs := typecheck.CheckPackage(fset, []*ast.File{depFile}, "example.com/tcmod/dep", depImp)
	if len(errs) != 0 {
		t.Fatalf("unexpected type errors checking dep: %v", errs)
	}

	blob, err := typecheck.WriteExport(depPkg, fset)
	if err != nil {
		t.Fatalf("WriteExport: %v", err)
	}

	userFile := parseTestdata(t, fset, "user/user.go")
	src := blobSource{blobs: map[string][]byte{"example.com/tcmod/dep": blob}}
	userImp := typecheck.NewImporter(fset, src, newStdlibExportSource(t), cache)
	userPkg, _, errs := typecheck.CheckPackage(fset, []*ast.File{userFile}, "example.com/tcmod/user", userImp)
	if len(errs) != 0 {
		t.Fatalf("unexpected type errors checking user: %v", errs)
	}
	if userPkg == nil || !userPkg.Complete() {
		t.Fatalf("expected a complete package, got %v", userPkg)
	}

	msg := userPkg.Scope().Lookup("Message")
	if msg == nil {
		t.Fatal("user.Message not found in package scope")
	}
}

// TestReadExport round-trips WriteExport's output through ReadExport
// directly, without going through an Importer.
func TestReadExport(t *testing.T) {
	fset := token.NewFileSet()
	depFile := parseTestdata(t, fset, "dep/dep.go")
	cache := typecheck.NewCache()
	imp := typecheck.NewImporter(fset, nil, newStdlibExportSource(t), cache)
	depPkg, _, errs := typecheck.CheckPackage(fset, []*ast.File{depFile}, "example.com/tcmod/dep", imp)
	if len(errs) != 0 {
		t.Fatalf("unexpected type errors: %v", errs)
	}

	blob, err := typecheck.WriteExport(depPkg, fset)
	if err != nil {
		t.Fatalf("WriteExport: %v", err)
	}

	readFset := token.NewFileSet()
	readCache := typecheck.NewCache()
	got, err := typecheck.ReadExport(blob, readFset, "example.com/tcmod/dep", readCache)
	if err != nil {
		t.Fatalf("ReadExport: %v", err)
	}
	if got.Scope().Lookup("Greet") == nil {
		t.Error("decoded package missing Greet")
	}
}

// TestReadExport_CachesSuccessWithoutRedecoding confirms a second
// ReadExport call for the same pkgPath against the same cache does not
// re-run gcexportdata.Read: Cache.Decodes() must stay at 1 after the
// second call.
func TestReadExport_CachesSuccessWithoutRedecoding(t *testing.T) {
	fset := token.NewFileSet()
	depFile := parseTestdata(t, fset, "dep/dep.go")
	writeCache := typecheck.NewCache()
	imp := typecheck.NewImporter(fset, nil, newStdlibExportSource(t), writeCache)
	depPkg, _, errs := typecheck.CheckPackage(fset, []*ast.File{depFile}, "example.com/tcmod/dep", imp)
	if len(errs) != 0 {
		t.Fatalf("unexpected type errors: %v", errs)
	}
	blob, err := typecheck.WriteExport(depPkg, fset)
	if err != nil {
		t.Fatalf("WriteExport: %v", err)
	}

	readFset := token.NewFileSet()
	readCache := typecheck.NewCache()
	if _, err := typecheck.ReadExport(blob, readFset, "example.com/tcmod/dep", readCache); err != nil {
		t.Fatalf("first ReadExport: %v", err)
	}
	if got := readCache.Decodes(); got != 1 {
		t.Fatalf("Decodes() after first ReadExport = %d, want 1", got)
	}
	if _, err := typecheck.ReadExport(blob, readFset, "example.com/tcmod/dep", readCache); err != nil {
		t.Fatalf("second ReadExport: %v", err)
	}
	if got := readCache.Decodes(); got != 1 {
		t.Errorf("Decodes() after second ReadExport = %d, want still 1 (repeat call must be served from cache.pkgs)", got)
	}
}

// TestReadExport_CachesFailure confirms a pkgPath whose export data fails
// to decode has that failure cached: a second call for the same pkgPath
// must return the SAME error without attempting gcexportdata.Read again,
// closing the field symptom of a ~1s decode cost repeating on every query
// for a package that can never successfully decode.
func TestReadExport_CachesFailure(t *testing.T) {
	fset := token.NewFileSet()
	cache := typecheck.NewCache()
	badData := []byte("not export data")

	_, err1 := typecheck.ReadExport(badData, fset, "example.com/broken", cache)
	if err1 == nil {
		t.Fatal("ReadExport with malformed data: got nil error, want a decode error")
	}
	if got := cache.FailedLen(); got != 1 {
		t.Fatalf("FailedLen() after first failed ReadExport = %d, want 1", got)
	}

	_, err2 := typecheck.ReadExport(badData, fset, "example.com/broken", cache)
	if err2 == nil || err2.Error() != err1.Error() {
		t.Errorf("second ReadExport error = %v, want the identical cached error %v", err2, err1)
	}
	if got := cache.FailedLen(); got != 1 {
		t.Errorf("FailedLen() after second failed ReadExport = %d, want still 1", got)
	}
}

// checkPkg parses rel, type-checks it as pkgPath against cache using src
// (nil to rely on stdlib alone) and stdlib as its two ExportSource tiers,
// and returns the resulting *types.Package.
func checkPkg(t *testing.T, fset *token.FileSet, cache *typecheck.Cache, stdlib typecheck.ExportSource, rel, pkgPath string, src typecheck.ExportSource) *types.Package {
	t.Helper()
	f := parseTestdata(t, fset, rel)
	imp := typecheck.NewImporter(fset, src, stdlib, cache)
	pkg, _, errs := typecheck.CheckPackage(fset, []*ast.File{f}, pkgPath, imp)
	if len(errs) != 0 {
		t.Fatalf("unexpected type errors checking %s: %v", pkgPath, errs)
	}
	return pkg
}

// writeBlob is typecheck.WriteExport, failing the test on error.
func writeBlob(t *testing.T, pkg *types.Package, fset *token.FileSet) []byte {
	t.Helper()
	blob, err := typecheck.WriteExport(pkg, fset)
	if err != nil {
		t.Fatalf("WriteExport(%s): %v", pkg.Path(), err)
	}
	return blob
}

// probeCache imports path through an Importer bound to (fset, cache) with
// every blob in all available, reporting the resulting *types.Package and
// whether it was served from cache (Decodes() unchanged) rather than
// freshly decoded.
func probeCache(t *testing.T, fset *token.FileSet, cache *typecheck.Cache, all typecheck.ExportSource, path string) (*types.Package, bool) {
	t.Helper()
	before := cache.Decodes()
	pkg, err := typecheck.NewImporter(fset, nil, all, cache).ImportFrom(path, "", 0)
	if err != nil {
		t.Fatalf("probe ImportFrom(%s): %v", path, err)
	}
	return pkg, cache.Decodes() == before
}

// invalidateFixturePaths are the import paths newInvalidateFixture's Cache
// holds entries or failures for.
type invalidateFixturePaths struct {
	dep, typedep, typeuser, unsafeuser, broken1, broken2 string
}

// newInvalidateFixture builds a Cache holding: dep (complete, unrelated to
// typedep/typeuser), unsafeuser (complete, unrelated to everything else —
// the negative control), typeuser (complete, whose deep export data embeds
// typedep because typeuser's own exported API returns a typedep.Greeting),
// and typedep itself, present only as the INCOMPLETE placeholder that
// decoding typeuser's self-contained blob leaves behind for it (see
// typedep's own testdata doc — gcexportdata only completes the top-level
// package a decode call was made for, see
// x/tools/internal/gcimporter/iimport.go's non-bundle pkgs[:1] completion
// loop). Two cached ReadExport failures (broken1, broken2) round out the
// fixture. Also returns fset (which any Cache.Invalidate result must be
// paired with), an ExportSource able to answer every non-failing path (for
// probeCache), and unsafeuser's own decoded pointer (for the
// pointer-identity assertion).
func newInvalidateFixture(t *testing.T) (fset *token.FileSet, cache *typecheck.Cache, all typecheck.ExportSource, unsafeuserPkg *types.Package, paths invalidateFixturePaths) {
	t.Helper()
	paths = invalidateFixturePaths{
		dep:        "example.com/tcmod/dep",
		typedep:    "example.com/tcmod/typedep",
		typeuser:   "example.com/tcmod/typeuser",
		unsafeuser: "example.com/tcmod/unsafeuser",
		broken1:    "example.com/broken1",
		broken2:    "example.com/broken2",
	}
	stdlib := newStdlibExportSource(t)

	// Build phase: a throwaway (buildFset, buildCache) pair produces the
	// checked packages and their WriteExport blobs, kept separate from the
	// (fset, cache) pair under test so that pair's decode order is fully
	// controlled (in particular, so typedep is never decoded there except
	// as a side effect of decoding typeuser).
	buildFset := token.NewFileSet()
	buildCache := typecheck.NewCache()
	depBlob := writeBlob(t, checkPkg(t, buildFset, buildCache, stdlib, "dep/dep.go", paths.dep, nil), buildFset)
	typedepBlob := writeBlob(t, checkPkg(t, buildFset, buildCache, stdlib, "typedep/typedep.go", paths.typedep, nil), buildFset)
	typedepSrc := blobSource{blobs: map[string][]byte{paths.typedep: typedepBlob}}
	typeuserBlob := writeBlob(t, checkPkg(t, buildFset, buildCache, stdlib, "typeuser/typeuser.go", paths.typeuser, typedepSrc), buildFset)
	unsafeuserBlob := writeBlob(t, checkPkg(t, buildFset, buildCache, stdlib, "unsafeuser/unsafeuser.go", paths.unsafeuser, nil), buildFset)

	all = blobSource{blobs: map[string][]byte{
		paths.dep:        depBlob,
		paths.typedep:    typedepBlob,
		paths.typeuser:   typeuserBlob,
		paths.unsafeuser: unsafeuserBlob,
	}}

	fset = token.NewFileSet()
	cache = typecheck.NewCache()
	if _, err := typecheck.ReadExport(depBlob, fset, paths.dep, cache); err != nil {
		t.Fatalf("ReadExport(dep): %v", err)
	}
	unsafeuserPkg, err := typecheck.ReadExport(unsafeuserBlob, fset, paths.unsafeuser, cache)
	if err != nil {
		t.Fatalf("ReadExport(unsafeuser): %v", err)
	}
	typeuserPkg, err := typecheck.ReadExport(typeuserBlob, fset, paths.typeuser, cache)
	if err != nil {
		t.Fatalf("ReadExport(typeuser): %v", err)
	}
	assertTypeuserEmbedsIncompleteTypedep(t, typeuserPkg, paths.typedep)

	if _, err := typecheck.ReadExport([]byte("not export data 1"), fset, paths.broken1, cache); err == nil {
		t.Fatal("ReadExport(broken1): want a decode error")
	}
	if _, err := typecheck.ReadExport([]byte("not export data 2"), fset, paths.broken2, cache); err == nil {
		t.Fatal("ReadExport(broken2): want a decode error")
	}
	return fset, cache, all, unsafeuserPkg, paths
}

// assertTypeuserEmbedsIncompleteTypedep confirms the fixture assumption
// newInvalidateFixture depends on: typeuserPkg's own deep export data
// manifest contains typedepPath, and that entry is still an INCOMPLETE
// placeholder.
func assertTypeuserEmbedsIncompleteTypedep(t *testing.T, typeuserPkg *types.Package, typedepPath string) {
	t.Helper()
	if !typeuserPkg.Complete() {
		t.Fatal("typeuser: want Complete() after its own ReadExport call")
	}
	for _, imp := range typeuserPkg.Imports() {
		if imp.Path() != typedepPath {
			continue
		}
		if imp.Complete() {
			t.Fatal("typedep: want an INCOMPLETE placeholder (never independently decoded); fixture assumption broken")
		}
		return
	}
	t.Fatal("typeuser.Imports() does not contain typedep; fixture assumption broken")
}

// assertInvalidateResult checks next — the result of some
// cache.Invalidate(changed) call — against wantPresent/wantAbsent path
// lists (see probeCache) and wantFailedLen.
func assertInvalidateResult(t *testing.T, fset *token.FileSet, next *typecheck.Cache, all typecheck.ExportSource, changed, wantPresent, wantAbsent []string, wantFailedLen int) {
	t.Helper()
	for _, p := range wantAbsent {
		if _, hit := probeCache(t, fset, next, all, p); hit {
			t.Errorf("%s: want absent (forced a fresh decode) after Invalidate(%v), got a cache hit", p, changed)
		}
	}
	for _, p := range wantPresent {
		if _, hit := probeCache(t, fset, next, all, p); !hit {
			t.Errorf("%s: want present (cache hit) after Invalidate(%v), got a fresh decode", p, changed)
		}
	}
	if got := next.FailedLen(); got != wantFailedLen {
		t.Errorf("FailedLen() after Invalidate(%v) = %d, want %d", changed, got, wantFailedLen)
	}
}

// TestCache_Invalidate exercises Cache.Invalidate against
// newInvalidateFixture's Cache.
func TestCache_Invalidate(t *testing.T) {
	fset, cache, all, unsafeuserPkg, paths := newInvalidateFixture(t)

	baseLen, baseFailedLen, baseBytes := cache.Len(), cache.FailedLen(), cache.Bytes()
	if baseLen != 4 {
		t.Fatalf("cache.Len() before any Invalidate call = %d, want 4 (dep, typeuser, typedep placeholder, unsafeuser)", baseLen)
	}
	if baseFailedLen != 2 {
		t.Fatalf("cache.FailedLen() before any Invalidate call = %d, want 2", baseFailedLen)
	}

	tests := []struct {
		name          string
		changed       []string
		wantPresent   []string
		wantAbsent    []string
		wantFailedLen int
	}{
		{
			name:          "changed path and the complete entry embedding it are both dropped",
			changed:       []string{paths.typedep},
			wantPresent:   []string{paths.dep, paths.unsafeuser},
			wantAbsent:    []string{paths.typedep, paths.typeuser},
			wantFailedLen: 2,
		},
		{
			name:          "incomplete placeholder dropped even when unrelated to changed",
			changed:       []string{paths.dep},
			wantPresent:   []string{paths.unsafeuser, paths.typeuser},
			wantAbsent:    []string{paths.dep, paths.typedep},
			wantFailedLen: 2,
		},
		{
			name:          "failure for a changed path dropped, failure for another path kept",
			changed:       []string{paths.broken1},
			wantPresent:   []string{paths.dep, paths.unsafeuser, paths.typeuser},
			wantAbsent:    []string{paths.typedep},
			wantFailedLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertInvalidateResult(t, fset, cache.Invalidate(tt.changed), all, tt.changed, tt.wantPresent, tt.wantAbsent, tt.wantFailedLen)
		})
	}

	t.Run("unrelated complete entry survives as the identical pointer", func(t *testing.T) {
		next := cache.Invalidate([]string{paths.typedep, paths.dep, paths.broken1})
		got, hit := probeCache(t, fset, next, all, paths.unsafeuser)
		if !hit {
			t.Fatal("unsafeuser: want a cache hit in the new Cache")
		}
		if got != unsafeuserPkg {
			t.Error("unsafeuser: want the SAME *types.Package pointer in the new Cache, got a different one")
		}
	})

	if got := cache.Len(); got != baseLen {
		t.Errorf("cache.Len() after Invalidate calls = %d, want unchanged %d (Invalidate must not mutate its receiver)", got, baseLen)
	}
	if got := cache.FailedLen(); got != baseFailedLen {
		t.Errorf("cache.FailedLen() after Invalidate calls = %d, want unchanged %d (Invalidate must not mutate its receiver)", got, baseFailedLen)
	}
	if got := cache.Bytes(); got != baseBytes {
		t.Errorf("cache.Bytes() after Invalidate calls = %d, want unchanged %d (Invalidate must not mutate its receiver)", got, baseBytes)
	}
}

// TestCheckPackage_CollectsErrors verifies every type error is collected,
// not just the first.
func TestCheckPackage_CollectsErrors(t *testing.T) {
	fset := token.NewFileSet()
	f := parseTestdata(t, fset, "broken/broken.go")
	cache := typecheck.NewCache()
	imp := typecheck.NewImporter(fset, nil, newStdlibExportSource(t), cache)

	pkg, _, errs := typecheck.CheckPackage(fset, []*ast.File{f}, "example.com/tcmod/broken", imp)
	if len(errs) == 0 {
		t.Fatal("expected at least one type error")
	}
	if pkg == nil {
		t.Fatal("expected a non-nil package even with type errors")
	}
}

// panicIfCalledSource is an ExportSource whose ExportData panics if ever
// invoked — a poison pill for TestImportFrom_UnsafeNeverReachesExportSource,
// proving ImportFrom("unsafe") short-circuits to types.Unsafe before
// consulting either configured ExportSource tier at all.
type panicIfCalledSource struct{}

func (panicIfCalledSource) ExportData(pkgPath string) ([]byte, bool, error) {
	panic(fmt.Sprintf("ExportData(%s) called: ImportFrom(\"unsafe\") should never reach an ExportSource", pkgPath))
}

// TestImportFrom_UnsafeNeverReachesExportSource verifies ImportFrom("unsafe")
// returns types.Unsafe directly, without asking either configured
// ExportSource for it: a real ExportSource (internal/depexport.Cache) that
// tried would panic trying to gcexportdata.Write(types.Unsafe) — see
// TestCheckPackage_DirectUnsafeImport for that failure mode reproduced
// end-to-end.
func TestImportFrom_UnsafeNeverReachesExportSource(t *testing.T) {
	fset := token.NewFileSet()
	imp := typecheck.NewImporter(fset, panicIfCalledSource{}, panicIfCalledSource{}, typecheck.NewCache())

	pkg, err := imp.ImportFrom("unsafe", "", 0)
	if err != nil {
		t.Fatalf("ImportFrom(unsafe): %v", err)
	}
	if pkg != types.Unsafe {
		t.Errorf("ImportFrom(unsafe) = %v, want types.Unsafe", pkg)
	}
}

// TestCheckPackage_DirectUnsafeImport checks unsafeuser.go, a workspace
// package that directly imports "unsafe" (mirroring the shape
// protoc-gen-go emits), through the same ExportSource shape
// internal/depexport.Cache uses (declaration-only check via
// internal/depcheck, then WriteExport — see stdlibExportSource.ExportData).
// Before ImportFrom special-cased "unsafe" (see its own doc), CheckPackage
// asking the fallback tier to resolve "unsafe" as an ordinary import
// reached WriteExport(types.Unsafe, ...), which panics unconditionally
// (gcexportdata's iexporter.pushDecl: "cannot export package unsafe") —
// this is the exact panic internal/index's own recover wrapper reported in
// production as "index: panic processing package ...: cannot export
// package unsafe" for any workspace package that imports "unsafe" directly.
func TestCheckPackage_DirectUnsafeImport(t *testing.T) {
	fset := token.NewFileSet()
	f := parseTestdata(t, fset, "unsafeuser/unsafeuser.go")
	imp := typecheck.NewImporter(fset, nil, newStdlibExportSource(t), typecheck.NewCache())

	pkg, _, errs := typecheck.CheckPackage(fset, []*ast.File{f}, "example.com/tcmod/unsafeuser", imp)
	if len(errs) != 0 {
		t.Fatalf("unexpected type errors: %v", errs)
	}
	if pkg == nil || !pkg.Complete() {
		t.Fatalf("expected a complete package, got %v", pkg)
	}
	if pkg.Scope().Lookup("AsPointer") == nil {
		t.Error("unsafeuser.AsPointer not found in package scope")
	}
}
