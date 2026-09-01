package index

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"github.com/sivchari/golance/internal/typecheck"
)

// staticImporter resolves every import from a fixed, already-checked
// *types.Package map — enough to type-check the tiny fixtures below without
// needing a real module or go/packages load.
type staticImporter struct {
	pkgs map[string]*types.Package
}

func (s *staticImporter) Import(path string) (*types.Package, error) {
	return s.ImportFrom(path, "", 0)
}

func (s *staticImporter) ImportFrom(path, _ string, _ types.ImportMode) (*types.Package, error) {
	if pkg, ok := s.pkgs[path]; ok {
		return pkg, nil
	}
	return nil, nil
}

// checkFixture parses src as pkgPath and type-checks it via imp, failing t
// on any error.
func checkFixture(t *testing.T, fset *token.FileSet, src, pkgPath string, imp types.ImporterFrom) (*types.Package, *ast.File) {
	t.Helper()
	f, err := parser.ParseFile(fset, pkgPath+".go", src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", pkgPath, err)
	}
	conf := types.Config{Importer: imp}
	pkg, err := conf.Check(pkgPath, fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatalf("check %s: %v", pkgPath, err)
	}
	return pkg, f
}

// TestMethodFingerprint_SurvivesBrokenCrossPackageIdentity is the empirical
// pin for a second, independent root cause behind the same production
// symptom registerMethodSet/implementingTypes's own fix targets: of a real
// monorepo's 71 LookupMethod candidates for an interface method, 62 were
// unexported (undecodable — the primary bug), but the remaining 9 WERE
// decodable and still failed types.Implements (survivors=0). Unexportedness
// alone cannot explain those 9.
//
// go/types compares two *types.Named values (e.g. an interface method's
// parameter/result type and a candidate's) by object identity, not
// structural shape: types.Implements/types.Identical requires the exact
// same *types.TypeName for a shared dependency type like gorm.io/gorm's
// *gorm.DB, which only holds if BOTH sides were decoded through the same
// gcexportdata imports map (internal/typecheck.Cache, shared via
// internal/xref.Resolver.cache for the whole lifetime of one Resolver).
// internal/xref.Resolver.Invalidate (added for reindex freshness, see its
// doc) deletes specific packages' cache entries whenever their content
// changes; a save-triggered reindex racing a slow, still-in-flight
// Implementation query (71 candidates' export data, each potentially
// pulling in a heavy dependency like gorm, is not instant) can delete and
// force a fresh re-decode of a package on ONE side of the comparison after
// the OTHER side already cached an older decode of the same shared
// dependency type — two different *types.Package objects for the same
// import path, breaking identity for every candidate decoded after the
// race despite genuinely implementing the interface.
//
// This proves the general phenomenon directly against this codebase's own
// decode path (internal/typecheck.WriteExport/ReadExport, exactly what
// internal/xref.Resolver.resolveNamed/resolveMethodFunc call) rather than
// raw gcexportdata: decoding an interface and a candidate through two
// independent internal/typecheck.Cache values (simulating the identity
// break above) makes types.Implements report a false negative, while
// MethodFingerprint — computed from each side's *types.Signature
// independently, comparing canonical fully-qualified text rather than
// object identity — still agrees. internal/xref.implementingTypes only
// ever needs the latter for its interface -> implementers confirmation
// (see its doc), so it is immune to this failure mode regardless of
// whether or how cache sharing breaks.
func TestMethodFingerprint_SurvivesBrokenCrossPackageIdentity(t *testing.T) {
	const sharedSrc = `package shared

type T struct{ X int }
`
	const ifaceSrc = `package iface

import "shared"

type Repo interface {
	NewDB() *shared.T
}
`
	const implSrc = `package impl

import "shared"

// DB is exported, so its export data decodes cleanly -- this pins the
// "decodable candidate still fails" symptom, distinct from the unexported-
// implementer fix (registerMethodSet/implementingTypes's own doc).
type DB struct{}

func (DB) NewDB() *shared.T { return nil }
`

	// One shared type-check pass for all three fixtures (a single fset/
	// importer map), exactly like a real build's own dependency-ordered
	// checking: shared must be checked once and handed to both iface and
	// impl, or neither would compile.
	checkFset := token.NewFileSet()
	sharedPkg, _ := checkFixture(t, checkFset, sharedSrc, "shared", &staticImporter{})
	ifacePkg, _ := checkFixture(t, checkFset, ifaceSrc, "iface", &staticImporter{pkgs: map[string]*types.Package{"shared": sharedPkg}})
	implPkg, _ := checkFixture(t, checkFset, implSrc, "impl", &staticImporter{pkgs: map[string]*types.Package{"shared": sharedPkg}})

	ifaceBlob, err := typecheck.WriteExport(ifacePkg, checkFset)
	if err != nil {
		t.Fatalf("WriteExport(iface): %v", err)
	}
	implBlob, err := typecheck.WriteExport(implPkg, checkFset)
	if err != nil {
		t.Fatalf("WriteExport(impl): %v", err)
	}

	// Decode each side through its OWN independent Cache/FileSet pair --
	// the identity-breaking condition (see the test's doc) -- rather than
	// through internal/xref.Resolver's single shared one, to isolate
	// exactly the property under test.
	ifaceFset := token.NewFileSet()
	ifaceDecoded, err := typecheck.ReadExport(ifaceBlob, ifaceFset, "iface", typecheck.NewCache())
	if err != nil {
		t.Fatalf("ReadExport(iface): %v", err)
	}
	implFset := token.NewFileSet()
	implDecoded, err := typecheck.ReadExport(implBlob, implFset, "impl", typecheck.NewCache())
	if err != nil {
		t.Fatalf("ReadExport(impl): %v", err)
	}

	repoNamed, ok := ifaceDecoded.Scope().Lookup("Repo").Type().(*types.Named)
	if !ok {
		t.Fatal("Repo is not a *types.Named after decode")
	}
	repoIface, ok := repoNamed.Underlying().(*types.Interface)
	if !ok {
		t.Fatal("Repo's underlying type is not an interface")
	}
	dbNamed, ok := implDecoded.Scope().Lookup("DB").Type().(*types.Named)
	if !ok {
		t.Fatal("DB is not a *types.Named after decode")
	}

	if types.Implements(types.NewPointer(dbNamed), repoIface) {
		t.Fatal("types.Implements(DB, Repo) = true across independently-decoded caches, want false: " +
			"this pin depends on go/types' own identity-based comparison actually breaking here; " +
			"if it no longer does, the second production root cause this guards against may need re-diagnosing")
	}

	// MethodFingerprint must agree regardless: both sides render "shared.T"
	// by its full import path (see fingerprintQualifier), a value that
	// depends only on the DECLARED signature's text, never on which
	// *types.Package object happened to decode it.
	repoSig, ok := methodSignature(repoIface, "NewDB")
	if !ok {
		t.Fatal("Repo.NewDB not found")
	}
	dbSig, ok := concreteMethodSignature(dbNamed, "NewDB")
	if !ok {
		t.Fatal("DB.NewDB not found")
	}
	repoFP := MethodFingerprint(repoSig)
	dbFP := MethodFingerprint(dbSig)
	if repoFP != dbFP {
		t.Errorf("MethodFingerprint mismatch across independently-decoded caches: iface=%d impl=%d, want equal (canonical signature text must not depend on decode identity)", repoFP, dbFP)
	}
	if repoFP == 0 || dbFP == 0 {
		t.Fatal("MethodFingerprint returned the generic-receiver sentinel (0) for a non-generic method")
	}
}

func methodSignature(iface *types.Interface, name string) (*types.Signature, bool) {
	for i := 0; i < iface.NumMethods(); i++ {
		fn := iface.Method(i)
		if fn.Name() == name {
			sig, ok := fn.Type().(*types.Signature)
			return sig, ok
		}
	}
	return nil, false
}

func concreteMethodSignature(named *types.Named, name string) (*types.Signature, bool) {
	ms := types.NewMethodSet(types.NewPointer(named))
	for i := 0; i < ms.Len(); i++ {
		fn, ok := ms.At(i).Obj().(*types.Func)
		if ok && fn.Name() == name {
			sig, ok := fn.Type().(*types.Signature)
			return sig, ok
		}
	}
	return nil, false
}
