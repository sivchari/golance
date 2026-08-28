package check

import "github.com/sivchari/golance/internal/graph"

// GraphSource adapts a *graph.Snapshot into a SnapshotSource by indexing
// its packages' GoFiles once at construction time.
type GraphSource struct {
	snap      *graph.Snapshot
	fileToPkg map[string]string
}

// NewGraphSource returns a SnapshotSource backed by snap.
func NewGraphSource(snap *graph.Snapshot) *GraphSource {
	fileToPkg := make(map[string]string)
	for pkgPath, pkg := range snap.Packages {
		for _, f := range pkg.GoFiles {
			fileToPkg[f] = pkgPath
		}
	}
	return &GraphSource{snap: snap, fileToPkg: fileToPkg}
}

// PackageForFile implements SnapshotSource.
func (g *GraphSource) PackageForFile(path string) (pkgPath, dir string, goFiles []string, ok bool) {
	pp, ok := g.fileToPkg[path]
	if !ok {
		return "", "", nil, false
	}
	pkg, ok := g.snap.Package(pp)
	if !ok {
		return "", "", nil, false
	}
	return pp, pkg.Dir, pkg.GoFiles, true
}
