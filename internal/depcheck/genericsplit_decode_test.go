package depcheck

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/sivchari/golance/internal/typecheck"
)

// blobMapSource is a typecheck.ExportSource backed by a fixed map, standing
// in for internal/depexport.Cache without the CAS/immutable-directory
// machinery this test does not need.
type blobMapSource struct{ blobs map[string][]byte }

func (s blobMapSource) ExportData(pkgPath string) (data []byte, ok bool, err error) {
	data, ok = s.blobs[pkgPath]
	return data, ok, nil
}

// TestGenericSplit_SurvivesIndependentBlobProduction determines whether the
// identity split TestProvider_ClosureScope_PreventsGenericTypeIdentitySplit
// guards against INSIDE one depcheck.Provider.Package call could also
// survive the WriteExport/ReadExport boundary a ROOT package's own
// typecheck.Importer uses in production (internal/index's Build): modspkg's
// blob is produced
// through one *depcheck.Provider (baking in ITS OWN "shared" resolution),
// shared's and bobpkg's blobs through a completely separate *depcheck.Provider
// instance -- simulating exactly what an LRU eviction between two independent
// depexport.Cache.checkAndPersist calls would produce, without needing to
// tune LRU capacity to force it -- and then decodes all three into ONE
// shared typecheck.Cache/imports map, exactly like internal/typecheck.Importer
// does for a real root package's direct dependencies.
//
// gcexportdata's own documented shallow-decode contract is to complete an
// existing (possibly incomplete) package entry in a shared imports map
// in place, by qualified name, regardless of which *types.Package instance
// originally produced the blob referencing it -- so if that contract holds
// here, this test's own "consumer" root package should type-check cleanly
// even though modspkg's blob was produced against a DIFFERENT "shared"
// generation than shared's own blob. A failure here means the split survives
// past depcheck.Provider's own internal LRU into the export-data blob
// boundary itself, and the fix must live at (or before) blob production, not
// only inside depcheck.Provider's cache lifetime.
func TestGenericSplit_SurvivesIndependentBlobProduction(t *testing.T) {
	meta := loadGenericSplitGraph(t)
	ctx := context.Background()

	// provider1 resolves "modspkg", which internally resolves "shared" as
	// its own nested dependency -- generation A, baked into modspkg's
	// exported MakeDefault signature.
	provider1 := NewProvider(meta, Options{})
	modspkgCP, err := provider1.Package(ctx, "example.com/genericsplit/modspkg")
	if err != nil {
		t.Fatalf("provider1.Package(modspkg): %v", err)
	}
	modspkgBlob, err := typecheck.WriteExport(modspkgCP.Types(), provider1.FileSet())
	if err != nil {
		t.Fatalf("WriteExport(modspkg): %v", err)
	}

	// provider2 is a completely independent Provider instance, so its own
	// "shared" and "bobpkg" resolutions are guaranteed non-identical to
	// provider1's -- generation B.
	provider2 := NewProvider(meta, Options{})
	sharedCP, err := provider2.Package(ctx, "example.com/genericsplit/shared")
	if err != nil {
		t.Fatalf("provider2.Package(shared): %v", err)
	}
	sharedBlob, err := typecheck.WriteExport(sharedCP.Types(), provider2.FileSet())
	if err != nil {
		t.Fatalf("WriteExport(shared): %v", err)
	}
	bobpkgCP, err := provider2.Package(ctx, "example.com/genericsplit/bobpkg")
	if err != nil {
		t.Fatalf("provider2.Package(bobpkg): %v", err)
	}
	bobpkgBlob, err := typecheck.WriteExport(bobpkgCP.Types(), provider2.FileSet())
	if err != nil {
		t.Fatalf("WriteExport(bobpkg): %v", err)
	}

	// Decode all three into ONE shared typecheck.Cache/fset -- exactly the
	// production shape internal/index.Build's single typecheck.Importer
	// gives every root package's own CheckPackage call.
	fset := token.NewFileSet()
	cache := typecheck.NewCache()
	src := blobMapSource{blobs: map[string][]byte{
		"example.com/genericsplit/modspkg": modspkgBlob,
		"example.com/genericsplit/shared":  sharedBlob,
		"example.com/genericsplit/bobpkg":  bobpkgBlob,
	}}
	imp := typecheck.NewImporter(fset, nil, src, cache)

	const consumerSrc = `package consumer

import (
	"example.com/genericsplit/bobpkg"
	"example.com/genericsplit/modspkg"
	"example.com/genericsplit/shared"
)

var _ bobpkg.Mod[shared.C] = modspkg.MakeDefault()
`
	f, err := parser.ParseFile(fset, "consumer.go", consumerSrc, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse consumer.go: %v", err)
	}

	pkg, _, errs := typecheck.CheckPackage(fset, []*ast.File{f}, "example.com/genericsplit/consumer", imp)
	if pkg == nil {
		t.Fatal("CheckPackage(consumer) produced no package")
	}
	if len(errs) > 0 {
		t.Errorf("CheckPackage(consumer) reported %d error(s) despite independently-produced-but-structurally-identical blobs -- "+
			"the split survives the WriteExport/ReadExport boundary, not just depcheck.Provider's own live check:", len(errs))
		for _, e := range errs {
			t.Errorf("  %v", e)
		}
	} else {
		t.Log("CheckPackage(consumer) reported no errors: the shared imports map self-healed the independently-produced blobs")
	}
}
