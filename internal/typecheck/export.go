package typecheck

import (
	"bytes"
	"fmt"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/gcexportdata"
)

// WriteExport encodes pkg's exported API as a self-contained blob, later
// decodable by ReadExport or by an Importer via ExportSource.
func WriteExport(pkg *types.Package, fset *token.FileSet) ([]byte, error) {
	var buf bytes.Buffer
	if err := gcexportdata.Write(&buf, fset, pkg); err != nil {
		return nil, fmt.Errorf("typecheck: write export data for %s: %w", pkg.Path(), err)
	}
	return buf.Bytes(), nil
}

// DuplicateImportPath reports the first import path reachable from tpkg
// through two non-identical *types.Package objects, or "" if none exists.
// Confirmed root cause of every observed WriteExport/ReadExport round-trip
// failure in production (five real corpus packages, one shared dependency
// cluster): gcexportdata's writer indexes referenced packages by object
// identity, not by path, so it happily serializes two manifest entries
// sharing one PkgPath when tpkg's own dependency resolution embedded two
// non-identical instances of it — an identity split somewhere upstream
// (see typecheck.Cache's own doc for the general class, and
// internal/depcheck.Provider's closureScope for its declaration-only-check
// counterpart). gcexportdata's own reader then panics decoding that
// manifest: golang.org/x/tools/internal/gcimporter.iimportCommon's "found
// duplicate PkgPaths" self-diagnostic (added for their own issue #63822)
// calls a hard-coded nil reportf in the public gcexportdata.Read/
// IImportData entry point, an unconditional nil-pointer dereference the
// moment it fires — confirmed by decoding real corpus blobs with x/tools'
// own panic recovery disabled. Callers should check this before calling
// WriteExport: it turns that opaque "internal error while importing ...:
// invalid memory address..." into an immediate, precisely-named
// diagnostic; WriteExport+ReadExport's own round-trip check remains the
// backstop for every other corruption shape.
//
// A breadth-first walk over Imports() (not a single flat scan) is
// necessary and sufficient: Imports() of a decoded, complete package
// reports its own full transitive reference set as a flat list (see
// gcimporter's ureader.go: "Imports() of pkg are all of the transitive
// packages that were loaded"), so walking one
// hop from every package already visited reaches every package tpkg's own
// exported declarations can possibly reference, without needing to walk
// tpkg's declarations/types directly.
func DuplicateImportPath(tpkg *types.Package) string {
	seen := map[string]*types.Package{tpkg.Path(): tpkg}
	queue := tpkg.Imports()
	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]
		if existing, ok := seen[pkg.Path()]; ok {
			if existing != pkg {
				return pkg.Path()
			}
			continue
		}
		seen[pkg.Path()] = pkg
		queue = append(queue, pkg.Imports()...)
	}
	return ""
}

// ReadExport decodes a blob written by WriteExport back into a
// *types.Package for pkgPath, adding position information to fset and
// registering the result in cache. Unlike a GOCACHE-generated export file,
// a WriteExport blob has no archive header, so it is passed directly to
// gcexportdata.Read rather than through gcexportdata.NewReader first.
//
// ReadExport is cheap to call repeatedly for the same pkgPath against the
// same cache: a prior successful decode is served straight from
// cache.pkgs without touching data again (mirroring Importer.cacheGet's
// short-circuit — gcexportdata.Read itself has no such check, so calling
// it unconditionally on every call would re-copy and re-parse data's
// manifest every time even though the expensive per-declaration decode it
// skips internally for an already-complete package is the only part that
// was actually free), and a prior FAILED decode is served from a cached
// error instead of repeating it. The latter matters because gcimporter
// recovers an internal panic into largely the same error shape a genuine
// EOF or malformed-input error takes (see iimportCommon's recover), so a
// bad blob's decode attempt costs roughly what a good one's full first
// decode does, every single call, without this: the field symptom this
// closes is an implementation/references query that costs about as long
// as a first decode on EVERY call for a workspace package whose export
// data cannot be decoded, because nothing remembered the earlier failure.
// A pkgPath's cached failure survives until cache.Invalidate names it (see
// the doc there), the same reindex-triggered invalidation a successful
// decode gets.
func ReadExport(data []byte, fset *token.FileSet, pkgPath string, cache *Cache) (*types.Package, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if pkg, ok := cache.pkgs[pkgPath]; ok && pkg.Complete() {
		return pkg, nil
	}
	if err, ok := cache.failed[pkgPath]; ok {
		return nil, err
	}
	pkg, err := gcexportdata.Read(bytes.NewReader(data), fset, cache.pkgs, pkgPath)
	if err != nil {
		wrapped := fmt.Errorf("typecheck: decode export data for %s: %w", pkgPath, err)
		cache.failed[pkgPath] = wrapped
		return nil, wrapped
	}
	cache.bytes += int64(len(data))
	cache.sizes[pkgPath] = int64(len(data))
	cache.decodes++
	return pkg, nil
}
