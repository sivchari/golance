package typecheck

import (
	"go/token"
	"go/types"
	"testing"
)

// twoInstancePackages returns two non-identical *types.Package objects both
// named path — the exact shape an identity split leaves behind: an
// otherwise-innocuous dependency resolved twice, producing two distinct
// *types.Named declarations for the same import path.
func twoInstancePackages(path string) (a, b *types.Package) {
	a = newPackageWithNamedT(path)
	b = newPackageWithNamedT(path)
	return a, b
}

// newPackageWithNamedT returns a complete *types.Package named path,
// declaring a single exported type T.
func newPackageWithNamedT(path string) *types.Package {
	pkg := types.NewPackage(path, "shared")
	tname := types.NewTypeName(token.NoPos, pkg, "T", nil)
	types.NewNamed(tname, types.NewStruct(nil, nil), nil)
	pkg.Scope().Insert(tname)
	pkg.MarkComplete()
	return pkg
}

// namedFrom returns pkg's own "T" declaration's *types.Named type.
func namedFrom(pkg *types.Package) *types.Named {
	return pkg.Scope().Lookup("T").Type().(*types.Named)
}

func TestDuplicateImportPath_DetectsSplitDependency(t *testing.T) {
	const sharedPath = "example.com/shared"
	a, b := twoInstancePackages(sharedPath)

	consumer := types.NewPackage("example.com/consumer", "consumer")
	consumer.Scope().Insert(types.NewVar(token.NoPos, consumer, "A", namedFrom(a)))
	consumer.Scope().Insert(types.NewVar(token.NoPos, consumer, "B", namedFrom(b)))
	consumer.SetImports([]*types.Package{a, b})
	consumer.MarkComplete()

	if got := DuplicateImportPath(consumer); got != sharedPath {
		t.Errorf("DuplicateImportPath(consumer) = %q, want %q", got, sharedPath)
	}
}

func TestDuplicateImportPath_CleanGraphReportsNone(t *testing.T) {
	shared := newPackageWithNamedT("example.com/shared")

	consumer := types.NewPackage("example.com/consumer", "consumer")
	consumer.Scope().Insert(types.NewVar(token.NoPos, consumer, "A", namedFrom(shared)))
	consumer.SetImports([]*types.Package{shared})
	consumer.MarkComplete()

	if got := DuplicateImportPath(consumer); got != "" {
		t.Errorf("DuplicateImportPath(consumer) = %q, want \"\" (single, consistent instance)", got)
	}
}

// TestDuplicateImportPath_ShapeAlsoFailsRoundTrip is corroborating evidence,
// not the primary regression pin: it confirms the shape
// TestDuplicateImportPath_DetectsSplitDependency catches is not a
// strawman — WriteExport/ReadExport round-tripping the same split
// dependency graph independently fails too (see DuplicateImportPath's own
// doc for the confirmed x/tools decode-site). The exact error text is
// x/tools-version-dependent, so only failure itself is asserted.
func TestDuplicateImportPath_ShapeAlsoFailsRoundTrip(t *testing.T) {
	const sharedPath = "example.com/shared"
	a, b := twoInstancePackages(sharedPath)

	consumer := types.NewPackage("example.com/consumer", "consumer")
	consumer.Scope().Insert(types.NewVar(token.NoPos, consumer, "A", namedFrom(a)))
	consumer.Scope().Insert(types.NewVar(token.NoPos, consumer, "B", namedFrom(b)))
	consumer.SetImports([]*types.Package{a, b})
	consumer.MarkComplete()

	fset := token.NewFileSet()
	blob, err := WriteExport(consumer, fset)
	if err != nil {
		t.Logf("WriteExport itself failed (acceptable — either write or read failing corroborates the shape is poisonous): %v", err)
		return
	}
	if _, err := ReadExport(blob, token.NewFileSet(), consumer.Path(), NewCache()); err != nil {
		t.Logf("ReadExport correctly failed to round-trip the split dependency: %v", err)
		return
	}
	t.Error("both WriteExport and ReadExport succeeded for a split-dependency graph — DuplicateImportPath may be over-eager relative to what actually corrupts a blob")
}
