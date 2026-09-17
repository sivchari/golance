package index

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/typecheck"
)

// The three packages below mirror internal/depcheck/testdata/genericsplit
// (github.com/stephenafamo/bob's Mod[T]/mods.Set[T] shape, see its own
// fixture doc): sharedSrc is a plain concrete type, modspkgSrc's
// MakeDefault returns a generic type instantiated against it, bobpkgSrc's
// Mod[T] interface is satisfied by modspkgSrc's Set[T] for any T.
const (
	exportSafetySharedPath  = "example.com/exportsafety/shared"
	exportSafetyBobpkgPath  = "example.com/exportsafety/bobpkg"
	exportSafetyModspkgPath = "example.com/exportsafety/modspkg"

	exportSafetySharedSrc = "package shared\n\ntype C struct{}\n"

	exportSafetyBobpkgSrc = "package bobpkg\n\ntype Mod[T any] interface {\n\tApply(T) error\n}\n"

	exportSafetyModspkgSrc = "package modspkg\n\nimport \"example.com/exportsafety/shared\"\n\ntype Set[T any] struct{}\n\nfunc (s *Set[T]) Apply(T) error { return nil }\n\nfunc MakeDefault() *Set[shared.C] { return &Set[shared.C]{} }\n"
)

func mustCheckExportSafetySrc(t *testing.T, fset *token.FileSet, pkgPath, src string, imp types.Importer) *types.Package {
	t.Helper()
	f, err := parser.ParseFile(fset, pkgPath+".go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", pkgPath, err)
	}
	conf := types.Config{Importer: imp}
	pkg, err := conf.Check(pkgPath, fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatalf("check %s: %v", pkgPath, err)
	}
	return pkg
}

func buildExportSafetyBlobs(t *testing.T) map[string][]byte {
	t.Helper()
	fset := token.NewFileSet()

	sharedPkg := mustCheckExportSafetySrc(t, fset, exportSafetySharedPath, exportSafetySharedSrc, nil)
	sharedBlob, err := typecheck.WriteExport(sharedPkg, fset)
	if err != nil {
		t.Fatalf("WriteExport(shared): %v", err)
	}
	bobpkgPkg := mustCheckExportSafetySrc(t, fset, exportSafetyBobpkgPath, exportSafetyBobpkgSrc, nil)
	bobpkgBlob, err := typecheck.WriteExport(bobpkgPkg, fset)
	if err != nil {
		t.Fatalf("WriteExport(bobpkg): %v", err)
	}
	modspkgImp := typecheck.NewImporter(fset, nil, blobSource{blobs: map[string][]byte{exportSafetySharedPath: sharedBlob}}, typecheck.NewCache())
	modspkgPkg := mustCheckExportSafetySrc(t, fset, exportSafetyModspkgPath, exportSafetyModspkgSrc, modspkgImp)
	modspkgBlob, err := typecheck.WriteExport(modspkgPkg, fset)
	if err != nil {
		t.Fatalf("WriteExport(modspkg): %v", err)
	}

	return map[string][]byte{
		exportSafetySharedPath:  sharedBlob,
		exportSafetyBobpkgPath:  bobpkgBlob,
		exportSafetyModspkgPath: modspkgBlob,
	}
}

// blobSource is a typecheck.ExportSource backed by a fixed map.
type blobSource struct{ blobs map[string][]byte }

func (s blobSource) ExportData(pkgPath string) ([]byte, bool, error) {
	b, ok := s.blobs[pkgPath]
	return b, ok, nil
}

// TestCheckOnePackage_ErrorTaintedCheckStillProducesExportWhenItRoundTrips is
// the regression pin against over-withholding: a package whose own check
// reports a real error must still get its export produced and used when
// that export actually round-trips cleanly — matching golance's own shipped
// v0.7.8 behavior, where the vast majority of error-tainted checks still
// decode fine. An earlier version of this fix withheld export data on
// errs>0 alone; that starved every dependent of hundreds of otherwise-
// healthy root packages of a perfectly usable export, regressing
// Stats.Incomplete roughly 4x on a real corpus. Only a blob that genuinely
// fails writeAndValidateExport's own round-trip check is withheld (see
// TestWriteAndValidateExport_PoisonBlobWithheld for that case). The error
// here is an ordinary body-level type mismatch inside an unexported
// helper, unrelated to any cross-package identity — Get's own exported
// signature stays perfectly valid, so WriteExport has nothing to trip on.
func TestCheckOnePackage_ErrorTaintedCheckStillProducesExportWhenItRoundTrips(t *testing.T) {
	fset := token.NewFileSet()
	imp := typecheck.NewImporter(fset, nil, blobSource{}, typecheck.NewCache())

	const pkgPath = "example.com/exportsafety/consumer"
	const consumerSrc = `package consumer

func helper() int {
	return "not an int"
}

func Get() int { return 1 }
`
	readFile := func(path string) ([]byte, error) {
		if path == "consumer.go" {
			return []byte(consumerSrc), nil
		}
		return nil, fmt.Errorf("not found: %s", path)
	}

	result, err := checkOnePackage(fset, imp, pkgPath, []string{"consumer.go"}, nil, readFile, "", false)
	if err != nil {
		t.Fatalf("checkOnePackage returned an error (want a graceful Incomplete degradation instead): %v", err)
	}
	if !result.Incomplete {
		t.Error("result.Incomplete = false, want true: helper's body has a real type error")
	}
	if result.FirstError == "" {
		t.Error("result.FirstError is empty, want the body-level type-mismatch error message")
	}
	if len(result.Export) == 0 {
		t.Error("result.Export is empty, want a produced export: this blob round-trips cleanly despite the check error, so it must still be used")
	}
	if len(result.Facts) == 0 {
		t.Error("result.Facts is empty, want a still-usable facts blob for navigation")
	}
}

// TestCheckOnePackage_DependentOfBrokenExportGetsCleanError confirms that a
// package importing a pkgPath with no usable export data on record (the
// state writeAndValidateExport's own poison case, see
// TestWriteAndValidateExport_PoisonBlobWithheld, leaves it in) fails with an
// ordinary "no export data" style error naming pkgPath, never a decode
// panic and never a poisoned dependent chain beyond that one clean
// Incomplete.
func TestCheckOnePackage_DependentOfBrokenExportGetsCleanError(t *testing.T) {
	blobs := buildExportSafetyBlobs(t)
	fset := token.NewFileSet()
	// No entry for "example.com/exportsafety/consumer" in src: exactly what
	// exp.Put would leave unset for an Incomplete package (see
	// checkAndStoreOutcome and casExportSource.ExportData's own miss path).
	imp := typecheck.NewImporter(fset, nil, blobSource{blobs: blobs}, typecheck.NewCache())

	const depSrc = `package dependent

import "example.com/exportsafety/consumer"

var _ = consumer.Get
`
	readFile := func(path string) ([]byte, error) {
		if path == "dependent.go" {
			return []byte(depSrc), nil
		}
		return nil, fmt.Errorf("not found: %s", path)
	}

	result, err := checkOnePackage(fset, imp, "example.com/exportsafety/dependent", []string{"dependent.go"}, nil, readFile, "", false)
	if err != nil {
		t.Fatalf("checkOnePackage returned an error (want a graceful Incomplete degradation instead): %v", err)
	}
	if !result.Incomplete {
		t.Fatal("result.Incomplete = false, want true: consumer has no export data to import")
	}
	if result.FirstError == "" {
		t.Fatal("result.FirstError is empty, want a clear \"no export data\" style message naming the broken import")
	}
	t.Logf("clean cascade error: %s", result.FirstError)
}

// TestWriteAndValidateExport_PoisonBlobWithheld is the regression pin for
// the actual poison case writeAndValidateExport's own round-trip check
// exists to catch: a tpkg whose exported API reaches a *types.Func with no
// valid *types.Signature (the shape go/types' own error recovery can leave
// behind for a generic instantiation's synthesized method when a call
// site's own generic-interface-satisfaction check fails) makes
// gcexportdata's writer panic — confirmed directly: go/types.NewFunc with a
// nil *Signature is a legal, if unusual, construction (go/types itself
// stores no typed-nil in this case, leaving Type() nil), and
// internal/gcimporter's own non-shallow doDecl unconditionally calls
// Signature.Recv() on whatever its own type assertion produces, a nil
// pointer dereference, not an internalError-typed panic, so
// iimportCommon's own recover re-panics it (see writeAndValidateExport's
// own doc). This constructs that shape directly via go/types' public API,
// independent of any particular type-check that might produce it in
// practice (typecheck.Cache's own append-only, generation-scoped design —
// see its doc — now closes the specific identity-split path this test used
// to drive through checkOnePackage's own Importer to reach the same taint).
func TestWriteAndValidateExport_PoisonBlobWithheld(t *testing.T) {
	fset := token.NewFileSet()
	pkg := types.NewPackage("example.com/exportsafety/poison", "poison")
	pkg.Scope().Insert(types.NewFunc(token.NoPos, pkg, "Broken", nil))
	pkg.MarkComplete()

	blob, err := writeAndValidateExport(pkg, fset)
	if err != nil {
		t.Logf("writeAndValidateExport correctly withheld the poison blob: %v", err)
		return
	}
	t.Errorf("writeAndValidateExport produced %d bytes with no error: this shape was expected to fail its own round-trip check", len(blob))
}

// TestWriteAndValidateExport_DuplicateImportPathWithheld is the regression
// pin for the confirmed real-world poison shape (see
// typecheck.DuplicateImportPath's own doc): tpkg reaches two non-identical
// *types.Package objects sharing one import path — an identity split
// somewhere in dependency resolution, not tpkg's own declarations. This
// must be caught (and reported with the split path name) before ever
// calling gcexportdata.Write, not only via the round-trip decode fallback.
func TestWriteAndValidateExport_DuplicateImportPathWithheld(t *testing.T) {
	const sharedPath = "example.com/exportsafety/shareddup"
	newInstance := func() *types.Package {
		pkg := types.NewPackage(sharedPath, "shareddup")
		tname := types.NewTypeName(token.NoPos, pkg, "T", nil)
		types.NewNamed(tname, types.NewStruct(nil, nil), nil)
		pkg.Scope().Insert(tname)
		pkg.MarkComplete()
		return pkg
	}
	a, b := newInstance(), newInstance()

	pkg := types.NewPackage("example.com/exportsafety/dupconsumer", "dupconsumer")
	pkg.Scope().Insert(types.NewVar(token.NoPos, pkg, "A", a.Scope().Lookup("T").Type()))
	pkg.Scope().Insert(types.NewVar(token.NoPos, pkg, "B", b.Scope().Lookup("T").Type()))
	pkg.SetImports([]*types.Package{a, b})
	pkg.MarkComplete()

	fset := token.NewFileSet()
	blob, err := writeAndValidateExport(pkg, fset)
	if err == nil {
		t.Fatalf("writeAndValidateExport produced %d bytes with no error: a split dependency on %q should have been withheld", len(blob), sharedPath)
	}
	if !strings.Contains(err.Error(), sharedPath) {
		t.Errorf("writeAndValidateExport error = %v, want it to name the split path %q", err, sharedPath)
	}
	t.Logf("writeAndValidateExport correctly withheld the split-dependency blob: %v", err)
}
