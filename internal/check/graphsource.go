package check

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/overlay"
)

// adhocPkgPathPrefix marks a pkgInfo.pkgPath synthesized by
// PackageForFile's ad-hoc fallback rather than resolved from the import
// graph, so downstream code that cares (currently nothing does; see
// PackageForFile's doc) can recognize it. It can never collide with a real
// Go import path, which is not URL-scheme-shaped.
const adhocPkgPathPrefix = "adhoc:"

// externalTestPkgPathMarker marks a pkgInfo.pkgPath synthesized by
// PackageForFile's external "_test" package detection: the suffix is
// appended to the base package's own real pkgPath, so Engine (via
// externalTestVariant) can recover which base package a given external
// test unit belongs to, and everything else (the facts index, xref, any
// lookup keyed by ws.snap.Packages) simply never finds a match for it — the
// same "excluded by construction, not by an explicit guard" property
// adhocPkgPathPrefix documents. " [external_test]" mirrors go/packages' own
// convention for a synthesized test-variant package ID (e.g. "p [p.test]")
// and can never collide with a real Go import path: neither a space nor
// square brackets are legal import path characters.
const externalTestPkgPathMarker = " [external_test]"

// externalTestVariant reports whether pkgPath was synthesized by
// PackageForFile's external test detection (see externalTestPkgPathMarker),
// and if so returns the base package's own real pkgPath it was derived
// from.
func externalTestVariant(pkgPath string) (basePkgPath string, ok bool) {
	return strings.CutSuffix(pkgPath, externalTestPkgPathMarker)
}

// GraphSource adapts a *graph.Snapshot into a SnapshotSource by indexing
// its packages' GoFiles once at construction time.
type GraphSource struct {
	snap      *graph.Snapshot
	reader    overlay.FileReader
	fileToPkg map[string]string
	dirToPkg  map[string]string
}

// NewGraphSource returns a SnapshotSource backed by snap. reader is used
// only by PackageForFile's ad-hoc and external-test fallbacks (see their
// docs), to read the package clause of a file that snap does not itself
// resolve.
func NewGraphSource(snap *graph.Snapshot, reader overlay.FileReader) *GraphSource {
	fileToPkg := make(map[string]string)
	dirToPkg := make(map[string]string, len(snap.Packages))
	for pkgPath, pkg := range snap.Packages {
		for _, f := range pkg.GoFiles {
			fileToPkg[f] = pkgPath
		}
		dirToPkg[pkg.Dir] = pkgPath
	}
	return &GraphSource{snap: snap, reader: reader, fileToPkg: fileToPkg, dirToPkg: dirToPkg}
}

// PackageForFile implements SnapshotSource. If path is not itself a known
// Go file, it first falls back to matching path's directory against a
// known package's Dir — covering an unsaved new file that a graph reload
// has not picked up yet, so it still joins its directory's package rather
// than getting no language features until saved. Within that directory
// fallback, a *_test.go file whose own package clause names the directory's
// package plus "_test" (the external test package Go itself only allows in
// a *_test.go-named file) resolves to a distinct unit instead of joining
// the base package: see externalTestPkgPathFor. If the directory fallback
// misses entirely — path's directory is not any known package's Dir
// either, e.g. a testdata/ fixture, a standalone script, or a GOROOT file
// opened after a stdlib jump — it falls back once more to synthesizing an
// ad-hoc pkgInfo from path itself: pkgPath is adhocPkgPathPrefix plus
// path's directory (a value that can never collide with a real import path
// and that no known package's Dir maps to, so it never collides with a
// real pkgPath either), dir is path's directory, and goFiles is left empty
// since no sibling is known ahead of time — Engine.resolveFiles's
// canonicalPackageName falls back to scanning the directory's own package
// clauses for that case already. This third fallback still returns
// ok=false if path has no readable or parseable package clause, so an
// empty buffer or a non-Go file continues to get no language features.
func (g *GraphSource) PackageForFile(path string) (pkgPath, dir string, goFiles []string, ok bool) {
	if pp, hit := g.fileToPkg[path]; hit {
		pkg, pkgOK := g.snap.Package(pp)
		if !pkgOK {
			return "", "", nil, false
		}
		return pp, pkg.Dir, pkg.GoFiles, true
	}
	pp, hit := g.dirToPkg[filepath.Dir(path)]
	if !hit {
		return g.adhocPackageForFile(path)
	}
	pkg, pkgOK := g.snap.Package(pp)
	if !pkgOK {
		return "", "", nil, false
	}
	if extPkgPath, extOK := g.externalTestPkgPathFor(path, pkg); extOK {
		return extPkgPath, pkg.Dir, nil, true
	}
	return pp, pkg.Dir, pkg.GoFiles, true
}

// externalTestPkgPathFor reports whether path — already known, via the
// directory fallback, to sit in pkg's directory without being one of pkg's
// own GoFiles — declares pkg's external "_test"-suffixed test package, and
// if so returns the synthesized pkgPath identifying that as its own Engine
// unit (see externalTestPkgPathMarker).
//
// The *_test.go name check runs first, before reading and parsing path's
// package clause: it is the only file name Go itself allows a
// "_test"-suffixed package clause in, so for every other file (an ordinary
// same-package sibling, or a brand-new unsaved file — the common case the
// directory fallback also serves, see PackageForFile's Phase 1 doc) it
// skips the read entirely instead of paying for one on every call just to
// find out the clause could not have matched anyway.
func (g *GraphSource) externalTestPkgPathFor(path string, pkg *graph.Package) (string, bool) {
	if !strings.HasSuffix(path, "_test.go") {
		return "", false
	}
	name, ok := g.packageClauseName(path)
	if !ok {
		return "", false
	}
	baseName, ok := g.basePackageName(pkg)
	if !ok || name != baseName+"_test" {
		return "", false
	}
	return pkg.ImportPath + externalTestPkgPathMarker, true
}

// basePackageName reads pkg's own declared Go package name, from the first
// of its GoFiles g.reader can still read and parse a package clause from.
// graph.Package deliberately carries no Name field of its own (see
// internal/graph's loadMode doc: graph never requests NeedSyntax/NeedTypes),
// so this is the only way to learn it. ok is false if pkg has no GoFiles at
// all (e.g. a directory holding only an external test package, with no
// base package of its own to speak of — see externalTestPkgPathFor, which
// then correctly declines to treat any file in it as an external test
// variant) or none of them are currently readable/parseable.
func (g *GraphSource) basePackageName(pkg *graph.Package) (string, bool) {
	for _, f := range pkg.GoFiles {
		if name, ok := g.packageClauseName(f); ok {
			return name, true
		}
	}
	return "", false
}

// packageClauseName reads path's package clause without parsing the rest of
// the file, via g.reader (overlay-aware). ok is false if reader is nil, or
// path cannot be read, or has no parseable package clause.
func (g *GraphSource) packageClauseName(path string) (string, bool) {
	if g.reader == nil {
		return "", false
	}
	src, err := g.reader.ReadFile(path)
	if err != nil {
		return "", false
	}
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.PackageClauseOnly)
	if err != nil || f == nil {
		return "", false
	}
	return f.Name.Name, true
}

// adhocPackageForFile is PackageForFile's final fallback; see its doc.
func (g *GraphSource) adhocPackageForFile(path string) (pkgPath, dir string, goFiles []string, ok bool) {
	if _, ok := g.packageClauseName(path); !ok {
		return "", "", nil, false
	}
	dir = filepath.Dir(path)
	return adhocPkgPathPrefix + dir, dir, nil, true
}
