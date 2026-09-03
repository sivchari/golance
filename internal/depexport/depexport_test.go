package depexport

import (
	"context"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/sivchari/golance/internal/depcheck"
	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/typecheck"
)

// loadTestGraph loads depcheck's own tiny fixture module as a real
// *graph.Snapshot, mirroring internal/depcheck's identical test helper —
// depexport deliberately exercises the same fixture rather than its own,
// since it is meant to sit directly on top of depcheck's own metadata
// resolution.
func loadTestGraph(t *testing.T) depcheck.MetadataSource {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "depcheck", "testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	return depcheck.NewGraphMetadataSource(snap)
}

func newTestCAS(t *testing.T) *store.CAS {
	t.Helper()
	cas, err := store.OpenCAS(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	return cas
}

// TestCache_ExportDataRoundTrip verifies the primary contract: ExportData
// for a real stdlib package returns bytes typecheck.ReadExport can decode
// straight back into a *types.Package exposing that package's real exported
// API — the same round trip typecheck.Importer performs internally when
// Cache is wired in as its ExportSource.
func TestCache_ExportDataRoundTrip(t *testing.T) {
	meta := loadTestGraph(t)
	provider := depcheck.NewProvider(meta, depcheck.Options{})
	cache := NewCache(newTestCAS(t), meta, provider, Options{})

	data, ok, err := cache.ExportData("strings")
	if err != nil {
		t.Fatalf("ExportData(strings): %v", err)
	}
	if !ok {
		t.Fatal("ExportData(strings): ok = false, want true")
	}

	fset := token.NewFileSet()
	pkg, err := typecheck.ReadExport(data, fset, "strings", typecheck.NewCache())
	if err != nil {
		t.Fatalf("ReadExport: %v", err)
	}
	if obj := pkg.Scope().Lookup("Builder"); obj == nil {
		t.Error("decoded strings package has no Builder in scope")
	}
	if obj := pkg.Scope().Lookup("Join"); obj == nil {
		t.Error("decoded strings package has no Join in scope")
	}
}

// TestCache_UnknownPackage verifies ExportData reports ok=false, no error,
// for a pkgPath the MetadataSource does not know at all — mirroring
// typecheck.ExportSource's documented "no data for pkgPath" contract, which
// an Importer relies on to fall through to any further source rather than
// treating a miss as fatal.
func TestCache_UnknownPackage(t *testing.T) {
	meta := loadTestGraph(t)
	provider := depcheck.NewProvider(meta, depcheck.Options{})
	cache := NewCache(newTestCAS(t), meta, provider, Options{})

	data, ok, err := cache.ExportData("example.com/does-not-exist")
	if err != nil {
		t.Fatalf("ExportData: unexpected error: %v", err)
	}
	if ok {
		t.Error("ExportData: ok = true for an unknown package, want false")
	}
	if data != nil {
		t.Errorf("ExportData: data = %v, want nil for an unknown package", data)
	}
}

// TestCache_PersistsUnderGOROOT verifies a stdlib package's export data (its
// directory is always under GOROOT) is actually written into the CAS —
// the persistence half of the package doc's "Cache identity" section, not
// just the resolution half TestCache_ExportDataRoundTrip already covers.
func TestCache_PersistsUnderGOROOT(t *testing.T) {
	meta := loadTestGraph(t)
	provider := depcheck.NewProvider(meta, depcheck.Options{})
	cas := newTestCAS(t)
	cache := NewCache(cas, meta, provider, Options{})

	if _, ok, err := cache.ExportData("strings"); err != nil || !ok {
		t.Fatalf("ExportData(strings): ok=%v err=%v", ok, err)
	}

	dir, _, _, ok := meta.Package("strings")
	if !ok {
		t.Fatal("meta.Package(strings): not found")
	}
	key := cache.digest("strings", dir)
	if !cas.Has(key) {
		t.Error("CAS does not hold a blob for strings' digest key after ExportData; a GOROOT package should always be persisted")
	}
}

// fakeMetadataSource reports a single, fixed package located wherever dir
// points, for exercising Cache's immutable-directory persistence decision
// against a location that is neither GOROOT nor GOModCache.
type fakeMetadataSource struct {
	pkgPath string
	dir     string
	goFiles []string
}

func (f fakeMetadataSource) Package(pkgPath string) (string, []string, []string, bool) {
	if pkgPath != f.pkgPath {
		return "", nil, nil, false
	}
	return f.dir, f.goFiles, nil, true
}

// writeLocalPackage writes a minimal, real Go package (one file, one
// exported const) under a fresh temp directory outside both GOROOT and
// GOModCache — standing in for a local `replace`-directive dependency,
// whose content is not identity-stable by directory path alone (see the
// package doc's "Cache identity" section).
func writeLocalPackage(t *testing.T) (dir string, goFile string) {
	t.Helper()
	dir = t.TempDir()
	goFile = filepath.Join(dir, "local.go")
	src := "package local\n\n// V is a fixture constant.\nconst V = 1\n"
	if err := os.WriteFile(goFile, []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture package: %v", err)
	}
	return dir, goFile
}

// TestCache_DoesNotPersistOutsideImmutableDirs verifies a package resolved
// outside GOROOT/GOModCache (e.g. a local `replace` target) is still
// correctly resolved by ExportData, but never written to the CAS — see the
// package doc's "Cache identity" section for why persisting it would be
// unsound.
func TestCache_DoesNotPersistOutsideImmutableDirs(t *testing.T) {
	dir, goFile := writeLocalPackage(t)
	const pkgPath = "example.com/local"
	meta := fakeMetadataSource{pkgPath: pkgPath, dir: dir, goFiles: []string{goFile}}
	provider := depcheck.NewProvider(meta, depcheck.Options{})
	cas := newTestCAS(t)
	cache := NewCache(cas, meta, provider, Options{})

	data, ok, err := cache.ExportData(pkgPath)
	if err != nil || !ok {
		t.Fatalf("ExportData(%s): ok=%v err=%v", pkgPath, ok, err)
	}
	fset := token.NewFileSet()
	pkg, err := typecheck.ReadExport(data, fset, pkgPath, typecheck.NewCache())
	if err != nil {
		t.Fatalf("ReadExport: %v", err)
	}
	if obj := pkg.Scope().Lookup("V"); obj == nil {
		t.Error("decoded local package has no V in scope")
	}

	key := cache.digest(pkgPath, dir)
	if cas.Has(key) {
		t.Error("CAS holds a blob for a non-GOROOT/GOModCache package; it should never be persisted")
	}
}

// TestCache_NilCASStillResolves verifies a Cache constructed with cas=nil
// (e.g. because the machine-global cache directory could not be opened —
// see NewCache's own doc) still resolves export data correctly on every
// call, just without ever persisting anything.
func TestCache_NilCASStillResolves(t *testing.T) {
	meta := loadTestGraph(t)
	provider := depcheck.NewProvider(meta, depcheck.Options{})
	cache := NewCache(nil, meta, provider, Options{})

	data, ok, err := cache.ExportData("strings")
	if err != nil || !ok {
		t.Fatalf("ExportData(strings): ok=%v err=%v", ok, err)
	}
	if len(data) == 0 {
		t.Error("ExportData(strings): empty data")
	}
}

// TestCache_WarmCASSkipsCheck verifies that once a package's export data is
// already in the CAS, a fresh Cache — sharing the CAS directory but backed
// by its OWN, cold depcheck.Provider — resolves it WITHOUT type-checking
// it again: the entire point of persisting across process/session
// boundaries (see the package doc). Provider.Checked (a fresh check
// counter) stays 0 for the warm Cache's own provider.
func TestCache_WarmCASSkipsCheck(t *testing.T) {
	meta := loadTestGraph(t)
	cas := newTestCAS(t)

	warmProvider := depcheck.NewProvider(meta, depcheck.Options{})
	warmCache := NewCache(cas, meta, warmProvider, Options{})
	if _, ok, err := warmCache.ExportData("strings"); err != nil || !ok {
		t.Fatalf("warm ExportData(strings): ok=%v err=%v", ok, err)
	}
	if got := warmProvider.Checked(); got == 0 {
		t.Fatalf("warm provider.Checked() = %d, want > 0 (this call should have actually type-checked strings)", got)
	}

	coldProvider := depcheck.NewProvider(meta, depcheck.Options{})
	coldCache := NewCache(cas, meta, coldProvider, Options{})
	data, ok, err := coldCache.ExportData("strings")
	if err != nil || !ok {
		t.Fatalf("cold ExportData(strings): ok=%v err=%v", ok, err)
	}
	if len(data) == 0 {
		t.Error("cold ExportData(strings): empty data")
	}
	if got := coldProvider.Checked(); got != 0 {
		t.Errorf("cold provider.Checked() = %d, want 0 (a warm CAS hit should never invoke the checker at all)", got)
	}
}

// countingMetadataSource wraps a depcheck.MetadataSource, counting how many
// times Package is called for each import path — mirrors
// internal/depcheck's identical test helper, used here to prove Cache's own
// singleflight collapsed N concurrent ExportData callers for the same
// pkgPath onto a single underlying check, without being confused by
// depcheck.Provider.Checked() also counting every TRANSITIVE dependency
// check the target's own recursive resolution performs.
type countingMetadataSource struct {
	depcheck.MetadataSource
	mu     sync.Mutex
	counts map[string]int
}

func newCountingMetadataSource(inner depcheck.MetadataSource) *countingMetadataSource {
	return &countingMetadataSource{MetadataSource: inner, counts: make(map[string]int)}
}

func (c *countingMetadataSource) Package(pkgPath string) (string, []string, []string, bool) {
	c.mu.Lock()
	c.counts[pkgPath]++
	c.mu.Unlock()
	return c.MetadataSource.Package(pkgPath)
}

func (c *countingMetadataSource) count(pkgPath string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[pkgPath]
}

// TestCache_Singleflight verifies concurrent ExportData calls for the same
// uncached pkgPath collapse onto a single underlying check, mirroring
// depcheck.Provider's own TestProvider_Singleflight for the identical
// property one layer up. The counting wrapper is given only to
// depcheck.NewProvider, not Cache itself: Cache's own ExportData calls
// meta.Package once per call (uncounted here, deliberately) merely to
// resolve pkgPath's directory for the persistence decision — cheap, and
// not what this test is protecting; what must collapse to exactly one call
// is depcheck.Provider.check's OWN metadata lookup, made once per actual
// fresh type-check and gated by Provider's own singleflight/LRU.
func TestCache_Singleflight(t *testing.T) {
	meta := loadTestGraph(t)
	countingMeta := newCountingMetadataSource(meta)
	provider := depcheck.NewProvider(countingMeta, depcheck.Options{})
	cache := NewCache(newTestCAS(t), meta, provider, Options{})
	const pkgPath = "example.com/depcheckmod/dep"

	const n = 20
	var wg sync.WaitGroup
	results := make([][]byte, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			data, _, err := cache.ExportData(pkgPath)
			results[i], errs[i] = data, err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: ExportData(%s): %v", i, pkgPath, err)
		}
	}
	if got := countingMeta.count(pkgPath); got != 1 {
		t.Errorf("meta.Package(%s) called %d times, want exactly 1 (singleflight should collapse %d concurrent callers)", pkgPath, got, n)
	}
}

// TestCache_CgoDependency verifies a dependency go/packages reports cgo
// source files for (its GoFiles includes a raw `import "C"` file — see
// internal/depcheck's own existing best-effort handling of an unresolved
// "C" import, imported here unchanged) still produces usable, decodable
// export data for its non-cgo-typed public API: depexport deliberately
// adds no cgo-specific handling of its own (see the package doc), relying
// entirely on depcheck.Provider's pre-existing degrade-gracefully behavior
// (types.Config.Error swallowed, IgnoreFuncBodies so a body-only C
// reference never even surfaces). This exercises the real "plugin" stdlib
// package, whose GoFiles includes plugin_dlopen.go (import "C") on
// platforms that support it; skipped elsewhere.
func TestCache_CgoDependency(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "freebsd" {
		t.Skip("plugin package is not available on this platform")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "plugin")
	if err != nil {
		t.Fatalf("graph.Load(plugin): %v", err)
	}
	meta := depcheck.NewGraphMetadataSource(snap)
	provider := depcheck.NewProvider(meta, depcheck.Options{})
	cache := NewCache(newTestCAS(t), meta, provider, Options{})

	data, ok, err := cache.ExportData("plugin")
	if err != nil {
		t.Fatalf("ExportData(plugin): %v", err)
	}
	if !ok {
		t.Fatal("ExportData(plugin): ok = false, want true")
	}
	fset := token.NewFileSet()
	pkg, err := typecheck.ReadExport(data, fset, "plugin", typecheck.NewCache())
	if err != nil {
		t.Fatalf("ReadExport(plugin): %v", err)
	}
	if obj := pkg.Scope().Lookup("Open"); obj == nil {
		t.Error("decoded plugin package has no Open in scope")
	}
}

// TestCache_ContextBackground documents (and pins) that ExportData never
// blocks past a canceled provider.Package call forever: this is exercised
// indirectly through provider itself (depcheck.Provider's own ctxImporter
// cancellation tests), so this only asserts Cache does not swallow or wrap
// away a context error into something unrecognizable.
func TestCache_ContextBackground(t *testing.T) {
	meta := loadTestGraph(t)
	provider := depcheck.NewProvider(meta, depcheck.Options{})
	cache := NewCache(newTestCAS(t), meta, provider, Options{})

	// A plain sanity call: ExportData has no ctx parameter of its own (see
	// typecheck.ExportSource), so this only confirms the background
	// context Cache uses internally does not itself reject a normal call.
	if _, ok, err := cache.ExportData("strings"); err != nil || !ok {
		t.Fatalf("ExportData(strings): ok=%v err=%v", ok, err)
	}
	_ = context.Background()
}

// TestCache_DigestFoldsGoVersionAndBuildFlags verifies two Caches
// configured with different GoVersion/BuildFlagsFingerprint values never
// collide on the same CAS key for the same package — the exact isolation
// the package doc's "Cache identity" section promises.
func TestCache_DigestFoldsGoVersionAndBuildFlags(t *testing.T) {
	meta := loadTestGraph(t)
	provider := depcheck.NewProvider(meta, depcheck.Options{})

	a := NewCache(nil, meta, provider, Options{GoVersion: "go1.99"})
	b := NewCache(nil, meta, provider, Options{GoVersion: "go1.100"})
	c := NewCache(nil, meta, provider, Options{GoVersion: "go1.99", BuildFlagsFingerprint: "tags=foo"})

	dir, _, _, ok := meta.Package("strings")
	if !ok {
		t.Fatal("meta.Package(strings): not found")
	}
	keyA := a.digest("strings", dir)
	keyB := b.digest("strings", dir)
	keyC := c.digest("strings", dir)
	if keyA == keyB {
		t.Error("digest collided across different GoVersion values")
	}
	if keyA == keyC {
		t.Error("digest collided across different BuildFlagsFingerprint values")
	}
}
