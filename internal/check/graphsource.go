package check

import (
	"go/parser"
	"go/token"
	"path/filepath"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/overlay"
)

// adhocPkgPathPrefix marks a pkgInfo.pkgPath synthesized by
// PackageForFile's ad-hoc fallback rather than resolved from the import
// graph, so downstream code that cares (currently nothing does; see
// PackageForFile's doc) can recognize it. It can never collide with a real
// Go import path, which is not URL-scheme-shaped.
const adhocPkgPathPrefix = "adhoc:"

// GraphSource adapts a *graph.Snapshot into a SnapshotSource by indexing
// its packages' GoFiles once at construction time.
type GraphSource struct {
	snap      *graph.Snapshot
	reader    overlay.FileReader
	fileToPkg map[string]string
	dirToPkg  map[string]string
}

// NewGraphSource returns a SnapshotSource backed by snap. reader is used
// only by PackageForFile's ad-hoc fallback (see its doc), to read the
// package clause of a file that is not part of any package snap knows
// about.
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
// than getting no language features until saved. If that also misses —
// path's directory is not any known package's Dir either, e.g. a
// testdata/ fixture, a standalone script, or a GOROOT file opened after a
// stdlib jump — it falls back once more to synthesizing an ad-hoc pkgInfo
// from path itself: pkgPath is adhocPkgPathPrefix plus path's directory (a
// value that can never collide with a real import path and that no known
// package's Dir maps to, so it never collides with a real pkgPath either),
// dir is path's directory, and goFiles is left empty since no sibling is
// known ahead of time — Engine.resolveFiles's canonicalPackageName falls
// back to scanning the directory's own package clauses for that case
// already. This third fallback still returns ok=false if path has no
// readable or parseable package clause, so an empty buffer or a non-Go
// file continues to get no language features.
func (g *GraphSource) PackageForFile(path string) (pkgPath, dir string, goFiles []string, ok bool) {
	pp, ok := g.fileToPkg[path]
	if !ok {
		pp, ok = g.dirToPkg[filepath.Dir(path)]
	}
	if ok {
		pkg, pkgOK := g.snap.Package(pp)
		if !pkgOK {
			return "", "", nil, false
		}
		return pp, pkg.Dir, pkg.GoFiles, true
	}
	return g.adhocPackageForFile(path)
}

// adhocPackageForFile is PackageForFile's final fallback; see its doc.
func (g *GraphSource) adhocPackageForFile(path string) (pkgPath, dir string, goFiles []string, ok bool) {
	if g.reader == nil {
		return "", "", nil, false
	}
	src, err := g.reader.ReadFile(path)
	if err != nil {
		return "", "", nil, false
	}
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.PackageClauseOnly)
	if err != nil || f == nil {
		return "", "", nil, false
	}
	dir = filepath.Dir(path)
	return adhocPkgPathPrefix + dir, dir, nil, true
}
