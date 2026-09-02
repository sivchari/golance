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
// A pkgPath's cached failure survives until cache.Delete(pkgPath) is
// called for it (see the doc there), the same reindex-triggered
// invalidation a successful decode gets.
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
	cache.decodes++
	return pkg, nil
}
