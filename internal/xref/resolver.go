package xref

import (
	"fmt"
	"go/token"
	"go/types"
	"strings"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/typecheck"
)

// Location identifies a span in a source file, in the byte-column
// coordinate system documented in the package doc.
type Location struct {
	File   string
	Line   uint32
	Col    uint32
	EndCol uint32
}

// SymbolInfo describes one workspace/symbol result.
type SymbolInfo struct {
	Name      string
	Kind      uint8  // one of the index.Kind* constants recorded in the facts blob
	Container string // defining package's import path
	Location  Location
}

// Edit is a single-line text replacement, in the same byte-column
// coordinate system as Location.
type Edit struct {
	Line, Col, EndCol uint32
	NewText           string
}

// Resolver answers cross-reference queries over db's per-root index, cas's
// content-addressed blobs, and snap's import graph. A Resolver decodes
// export data through one shared internal/typecheck.Cache and
// token.FileSet for its lifetime (see package doc); construct a fresh one
// to bound their growth for a long session.
type Resolver struct {
	db   *store.DB
	cas  *store.CAS
	snap *graph.Snapshot
	fset *token.FileSet

	cache *typecheck.Cache

	fileToPkg     map[string]string
	pkgPathByHash map[uint64]string

	root     string // snap.Dir(); the base a relative-format stored path joins onto
	relative bool   // whether db's UnitPointers and cas's blobs store paths relative to root
}

// New returns a Resolver over db's per-root index, cas's content-addressed
// blobs, and snap's import graph. relative must match the
// internal/index.Options.RelativePaths value they were built or reindexed
// with (see internal/server.RelativeIndexPaths, the source of truth for
// that decision): it tells New whether a stored file path needs joining
// back onto snap.Dir() to become the absolute path every Location this
// Resolver returns must carry.
func New(db *store.DB, cas *store.CAS, snap *graph.Snapshot, relative bool) *Resolver {
	fileToPkg := make(map[string]string)
	pkgPathByHash := make(map[uint64]string, len(snap.Packages))
	for path, pkg := range snap.Packages {
		pkgPathByHash[store.Hash(path)] = path
		for _, f := range pkg.GoFiles {
			fileToPkg[f] = path
		}
	}
	return &Resolver{
		db:            db,
		cas:           cas,
		snap:          snap,
		fset:          token.NewFileSet(),
		cache:         typecheck.NewCache(),
		fileToPkg:     fileToPkg,
		pkgPathByHash: pkgPathByHash,
		root:          snap.Dir(),
		relative:      relative,
	}
}

// unitBlob loads and decodes pkgHash's current [store.UnitBlob] via db's
// UnitPointer and cas. It returns [store.ErrNotFound] if pkgHash has never
// been indexed.
func (r *Resolver) unitBlob(pkgHash uint64) (store.UnitBlob, error) {
	ptr, err := r.db.GetUnit(pkgHash)
	if err != nil {
		return store.UnitBlob{}, err
	}
	blob, ok, err := r.cas.Get(ptr.BlobKey)
	if err != nil {
		return store.UnitBlob{}, err
	}
	if !ok {
		return store.UnitBlob{}, store.ErrNotFound
	}
	return store.DecodeUnitBlob(blob)
}

// resolvedSymbol identifies one symbol definition: the defining package's
// hash, its SymbolID hash within that package's facts, its kind, and its
// name.
type resolvedSymbol struct {
	PkgHash uint64
	IDHash  uint64
	Kind    uint8
	Name    string
}

// resolveAt resolves the symbol at (file, line, col): a reference there
// resolves to what it points to; a definition there resolves to itself.
func (r *Resolver) resolveAt(file string, line, col uint32) (resolvedSymbol, error) {
	pkgPath, ok := r.fileToPkg[file]
	if !ok {
		return resolvedSymbol{}, fmt.Errorf("xref: %s is not part of any known package", file)
	}
	pkgHash := store.Hash(pkgPath)

	u, err := r.unitBlob(pkgHash)
	if err != nil {
		return resolvedSymbol{}, fmt.Errorf("xref: read facts for %s: %w", pkgPath, err)
	}
	v, err := store.NewView(u.Facts)
	if err != nil {
		return resolvedSymbol{}, err
	}
	fileIdx, ok := r.fileIndexOf(v, file)
	if !ok {
		return resolvedSymbol{}, fmt.Errorf("xref: %s has no entry in %s's facts", file, pkgPath)
	}

	if ref, ok := v.RefsAt(fileIdx, line, col); ok {
		out, found := r.resolveRefTarget(ref)
		if !found {
			return resolvedSymbol{}, fmt.Errorf("xref: no symbol at %s:%d:%d", file, line, col)
		}
		return out, nil
	}
	if s, ok := symbolAtPosition(v, fileIdx, line, col); ok {
		return resolvedSymbol{PkgHash: pkgHash, IDHash: s.IDHash(), Kind: s.Kind(), Name: s.Name()}, nil
	}
	return resolvedSymbol{}, fmt.Errorf("xref: no symbol at %s:%d:%d", file, line, col)
}

// resolveRefTarget looks up ref's target symbol's kind and name from its
// defining package's facts.
func (r *Resolver) resolveRefTarget(ref store.Ref) (resolvedSymbol, bool) {
	name, kind, _, ok := r.symbolByHash(ref.ToPkgHash(), ref.ToSymbolIDHash())
	if !ok {
		return resolvedSymbol{}, false
	}
	return resolvedSymbol{PkgHash: ref.ToPkgHash(), IDHash: ref.ToSymbolIDHash(), Kind: kind, Name: name}, true
}

// symbolByHash returns the name, kind, and declaration location recorded for
// idHash in pkgHash's facts blob.
func (r *Resolver) symbolByHash(pkgHash, idHash uint64) (name string, kind uint8, loc Location, ok bool) {
	u, err := r.unitBlob(pkgHash)
	if err != nil {
		return "", 0, Location{}, false
	}
	v, err := store.NewView(u.Facts)
	if err != nil {
		return "", 0, Location{}, false
	}
	s, found := v.LookupSymbol(idHash)
	if !found {
		return "", 0, Location{}, false
	}
	path, err := v.FileAt(int(s.FileIdx()))
	if err != nil {
		return "", 0, Location{}, false
	}
	loc = Location{File: absPath(r.root, path, r.relative), Line: s.Line(), Col: s.Col(), EndCol: s.Col() + uint32(len(s.Name()))}
	return s.Name(), s.Kind(), loc, true
}

// resolveNamed decodes pkgPath's export data through r's shared cache and
// looks up name in its package scope.
func (r *Resolver) resolveNamed(pkgPath, name string) (*types.Named, error) {
	u, err := r.unitBlob(store.Hash(pkgPath))
	if err != nil {
		return nil, fmt.Errorf("xref: read export data for %s: %w", pkgPath, err)
	}
	tpkg, err := typecheck.ReadExport(u.Export, r.fset, pkgPath, r.cache)
	if err != nil {
		return nil, fmt.Errorf("xref: decode export data for %s: %w", pkgPath, err)
	}
	obj := tpkg.Scope().Lookup(name)
	if obj == nil {
		return nil, fmt.Errorf("xref: %s not found in package %s export data", name, pkgPath)
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return nil, fmt.Errorf("xref: %s in package %s is not a type", name, pkgPath)
	}
	named, ok := tn.Type().(*types.Named)
	if !ok {
		return nil, fmt.Errorf("xref: %s in package %s is not a named type", name, pkgPath)
	}
	return named, nil
}

// fileIndexOf returns the index of file (an absolute path) in v's file
// table, converting it to the same root-relative form the table stores
// first when r.relative is set.
func (r *Resolver) fileIndexOf(v *store.View, file string) (uint32, bool) {
	key := file
	if r.relative {
		key = relPath(r.root, file)
	}
	for i := 0; i < v.FileCount(); i++ {
		f, err := v.FileAt(i)
		if err == nil && f == key {
			return uint32(i), true
		}
	}
	return 0, false
}

// symbolAtPosition scans v's symbol table for a definition at (fileIdx,
// line) whose identifier span contains col.
func symbolAtPosition(v *store.View, fileIdx, line, col uint32) (store.Symbol, bool) {
	for i := 0; i < v.SymbolCount(); i++ {
		s, err := v.SymbolAt(i)
		if err != nil {
			continue
		}
		if s.FileIdx() != fileIdx || s.Line() != line {
			continue
		}
		end := s.Col() + uint32(len(s.Name()))
		if col >= s.Col() && col < end {
			return s, true
		}
	}
	return store.Symbol{}, false
}

// splitSymbolID splits a SymbolID string (as built by [store.BuildSymbolID])
// back into its package path and objectpath.
func splitSymbolID(sid string) (pkgPath, objPath string, ok bool) {
	i := strings.Index(sid, "#")
	if i < 0 {
		return "", "", false
	}
	return sid[:i], sid[i+1:], true
}
