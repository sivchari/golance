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
func ReadExport(data []byte, fset *token.FileSet, pkgPath string, cache *Cache) (*types.Package, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	pkg, err := gcexportdata.Read(bytes.NewReader(data), fset, cache.pkgs, pkgPath)
	if err != nil {
		return nil, fmt.Errorf("typecheck: decode export data for %s: %w", pkgPath, err)
	}
	return pkg, nil
}
