package check

import (
	"path/filepath"

	"github.com/sivchari/golance/internal/graph"
)

// GraphSource adapts a *graph.Snapshot into a SnapshotSource by indexing
// its packages' GoFiles once at construction time.
type GraphSource struct {
	snap      *graph.Snapshot
	fileToPkg map[string]string
	dirToPkg  map[string]string
}

// NewGraphSource returns a SnapshotSource backed by snap.
func NewGraphSource(snap *graph.Snapshot) *GraphSource {
	fileToPkg := make(map[string]string)
	dirToPkg := make(map[string]string, len(snap.Packages))
	for pkgPath, pkg := range snap.Packages {
		for _, f := range pkg.GoFiles {
			fileToPkg[f] = pkgPath
		}
		dirToPkg[pkg.Dir] = pkgPath
	}
	return &GraphSource{snap: snap, fileToPkg: fileToPkg, dirToPkg: dirToPkg}
}

// PackageForFile implements SnapshotSource. If path is not itself a known
// Go file, it falls back to matching path's directory against a known
// package's Dir — covering an unsaved new file that a graph reload has not
// picked up yet, so it still joins its directory's package rather than
// getting no language features until saved.
func (g *GraphSource) PackageForFile(path string) (pkgPath, dir string, goFiles []string, ok bool) {
	pp, ok := g.fileToPkg[path]
	if !ok {
		pp, ok = g.dirToPkg[filepath.Dir(path)]
	}
	if !ok {
		return "", "", nil, false
	}
	pkg, ok := g.snap.Package(pp)
	if !ok {
		return "", "", nil, false
	}
	return pp, pkg.Dir, pkg.GoFiles, true
}
