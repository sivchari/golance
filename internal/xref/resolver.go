package xref

import (
	"context"
	"fmt"
	"go/token"
	"go/types"
	"log"
	"math"
	"path/filepath"
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
	units *unitCache

	fileToPkg     map[string]string
	dirToPkg      map[string]string
	pkgPathByHash map[uint64]string

	root     string // snap.Dir(); the base a relative-format stored path joins onto
	relative bool   // whether db's UnitPointers and cas's blobs store paths relative to root

	logger *log.Logger // server-side diagnostics only; see SetLogger
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
	dirToPkg := make(map[string]string, len(snap.Packages))
	pkgPathByHash := make(map[uint64]string, len(snap.Packages))
	for path, pkg := range snap.Packages {
		pkgPathByHash[store.Hash(path)] = path
		// fileToPkg/dirToPkg deliberately index root (workspace) packages
		// only, even though snap.Packages also carries the whole transitive
		// closure (module dependencies and the standard library — see
		// internal/graph's loadMode doc): the facts index this Resolver
		// reads from only ever covers root packages
		// (internal/index/scheduler.go's doc), so a dependency file's own
		// pkgPath would resolve here only to send resolveAt straight into a
		// GetUnit call that can never succeed. Excluding it here instead
		// makes pkgPathForFile report "not part of any known package" for a
		// dependency file exactly like it already does for a genuinely
		// unresolvable one (e.g. a testdata fixture) — the same ordinary,
		// low-noise miss internal/server.definitionFallback's ad-hoc
		// CheckedPackage/SamePackageDefinition/DependencyDefinition chain
		// already answers on its own, rather than a wrapped "read facts for
		// X: store: not found" that reads like a genuine index failure for
		// what is, for every dependency file, an entirely expected outcome.
		if !pkg.Root {
			continue
		}
		for _, f := range pkg.GoFiles {
			fileToPkg[f] = path
		}
		dirToPkg[pkg.Dir] = path
	}
	return &Resolver{
		db:            db,
		cas:           cas,
		snap:          snap,
		fset:          token.NewFileSet(),
		cache:         typecheck.NewCache(),
		units:         newUnitCache(),
		fileToPkg:     fileToPkg,
		dirToPkg:      dirToPkg,
		pkgPathByHash: pkgPathByHash,
		root:          snap.Dir(),
		relative:      relative,
	}
}

// unitBlob loads and decodes pkgHash's current [store.UnitBlob] via db's
// UnitPointer and cas, serving a repeat call for the same content from
// r.units instead of re-reading and re-decoding the CAS blob. It returns
// [store.ErrNotFound] if pkgHash has never been indexed. A canceled ctx
// surfaces as ctx's own error (see [store.DB.GetUnit] and [store.CAS.Get]),
// letting a caller distinguish a real cancellation from an ordinary "not
// found" miss.
//
// db.GetUnit's own bbolt read always runs first, even on what turns out to
// be a cache hit: it is the only way to learn pkgHash's CURRENT BlobKey,
// which is what r.units is keyed by (see its doc) — cheap relative to the
// os.ReadFile plus decode a cache hit then goes on to skip.
func (r *Resolver) unitBlob(ctx context.Context, pkgHash uint64) (store.UnitBlob, error) {
	ptr, err := r.db.GetUnit(ctx, pkgHash)
	if err != nil {
		return store.UnitBlob{}, err
	}
	if u, ok := r.units.get(ptr.BlobKey); ok {
		return u, nil
	}
	blob, ok, err := r.cas.Get(ctx, ptr.BlobKey)
	if err != nil {
		return store.UnitBlob{}, err
	}
	if !ok {
		return store.UnitBlob{}, store.ErrNotFound
	}
	u, err := store.DecodeUnitBlob(blob)
	if err != nil {
		return store.UnitBlob{}, err
	}
	r.units.put(ptr.BlobKey, u)
	return u, nil
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
func (r *Resolver) resolveAt(ctx context.Context, file string, line, col uint32) (resolvedSymbol, error) {
	pkgPath, ok := r.pkgPathForFile(file)
	if !ok {
		return resolvedSymbol{}, fmt.Errorf("xref: %s is not part of any known package", file)
	}
	pkgHash := store.Hash(pkgPath)

	u, err := r.unitBlob(ctx, pkgHash)
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
		out, found := r.resolveRefTarget(ctx, ref)
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
func (r *Resolver) resolveRefTarget(ctx context.Context, ref store.Ref) (resolvedSymbol, bool) {
	name, kind, _, ok := r.symbolByHash(ctx, ref.ToPkgHash(), ref.ToSymbolIDHash())
	if !ok {
		return resolvedSymbol{}, false
	}
	return resolvedSymbol{PkgHash: ref.ToPkgHash(), IDHash: ref.ToSymbolIDHash(), Kind: kind, Name: name}, true
}

// symbolByHash returns the name, kind, and declaration location recorded for
// idHash in pkgHash's facts blob.
func (r *Resolver) symbolByHash(ctx context.Context, pkgHash, idHash uint64) (name string, kind uint8, loc Location, ok bool) {
	u, err := r.unitBlob(ctx, pkgHash)
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
	loc = Location{File: absPath(r.root, path, r.relative), Line: s.Line(), Col: s.Col(), EndCol: s.Col() + u32len(len(s.Name()))}
	return s.Name(), s.Kind(), loc, true
}

// resolveNamed decodes pkgPath's export data through r's shared cache and
// looks up name in its package scope.
func (r *Resolver) resolveNamed(ctx context.Context, pkgPath, name string) (*types.Named, error) {
	u, err := r.unitBlob(ctx, store.Hash(pkgPath))
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

// SetLogger installs l as r's diagnostic logger for implementation-query
// failures (see implementation.go's implDiag/logImplDiag): an empty
// "Go to Implementation" result is otherwise indistinguishable, from the
// client's point of view, between "genuinely no implementers" and a
// resolution step silently failing along the way (e.g. a candidate or the
// queried interface itself whose export data could not be decoded -- the
// exact symptom a real monorepo report traced to an interface method
// signature referencing a module dependency's type). r logs nowhere until
// this is called (the zero value), so every existing New call site --
// production and this package's own test helpers alike -- keeps its
// current four-argument shape and its current silent behavior unless a
// caller opts in.
func (r *Resolver) SetLogger(l *log.Logger) {
	r.logger = l
}

// Invalidate drops pkgPaths' decoded *types.Package entries from r's shared
// export-data cache, so a later resolveNamed/resolveMethodFunc call
// re-decodes fresh export data instead of silently reusing a *types.Package
// decoded before pkgPaths' facts were last reindexed.
//
// This matters because r.cache is shared across every query for r's whole
// lifetime (see the Resolver doc), while the underlying CAS blob a given
// pkgPath's facts live in changes independently, via a didSave-triggered
// [internal/index.Reindex] running concurrently with query handling.
// gcexportdata.Read (which resolveNamed/resolveMethodFunc call through
// [typecheck.ReadExport]) returns whatever *types.Package is already in the
// imports map it is given for a path, without even looking at the newly
// read bytes, once that path has been decoded into the map once — so
// without this, a package queried once early in a session (e.g. an
// interface whose method set later gains or loses a method) would keep
// answering from that first decode forever, regardless of how many times
// it is actually reindexed afterward. That is the "Go to Implementation
// alternates between working and not" instability this exists to close:
// whether a given package's cache entry happens to already be warm from an
// earlier query is invisible to the caller, so the same query can look
// flaky depending only on session history.
//
// Callers pass pkgPath plus its reverse-dependency closure ([graph.
// Snapshot.ClosureUnits]), mirroring depCacheHolder.invalidate: an
// importer's own decoded *types.Package can embed direct references to
// pkgPath's now-superseded one (e.g. an embedded interface or struct
// field), so it must be dropped and re-decoded too, even though its own
// source content did not change.
//
// r.units needs no equivalent treatment here: it is keyed by BlobKey, the
// CAS content address a reindex necessarily changes (see unitCache's doc),
// so a stale entry simply stops being looked up on its own rather than
// needing an explicit drop.
func (r *Resolver) Invalidate(pkgPaths []string) {
	for _, p := range pkgPaths {
		r.cache.Delete(p)
	}
}

// pkgPathForFile resolves file to its containing package's import path,
// among root (workspace) packages only — see New's doc for why a
// dependency file is deliberately never found here. r.fileToPkg (built from
// graph.Package.GoFiles) never lists an in-package _test.go file at all —
// internal/graph's loadMode loads without packages.Config.Tests, the same
// gap testFilesInPackage exists to close for the facts index itself (see
// internal/index/testfiles.go) — so a position inside one always misses
// there. This falls back to matching file's directory against a known
// package's Dir, mirroring internal/check.GraphSource.PackageForFile's
// identical fallback and internal/server.workspace's dirToPkg.
//
// The directory fallback alone is not enough to trust file: it also
// matches an external "_test"-suffixed test package file or an unrelated
// ad-hoc file sitting in the same directory, neither of which
// testFilesInPackage folds into the unit's facts. Rather than duplicate
// that package-clause filtering here, this returns the directory's
// candidate pkgPath as-is and lets resolveAt's subsequent fileIndexOf
// lookup against the unit's own facts file table — the source of truth for
// what was actually indexed — reject file if it never made it in.
func (r *Resolver) pkgPathForFile(file string) (string, bool) {
	if pkgPath, ok := r.fileToPkg[file]; ok {
		return pkgPath, true
	}
	pkgPath, ok := r.dirToPkg[filepath.Dir(file)]
	return pkgPath, ok
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
		end := s.Col() + u32len(len(s.Name()))
		if col >= s.Col() && col < end {
			return s, true
		}
	}
	return store.Symbol{}, false
}

// u32len returns n — always a len() of a symbol name, a source identifier
// that can never plausibly approach 4 GiB — as a uint32, panicking if n is
// negative or exceeds math.MaxUint32. Hitting the panic would mean facts
// data violates an invariant enforced when it was written (see
// internal/store's own u32len), a programmer error rather than
// recoverable untrusted input.
func u32len(n int) uint32 {
	if n < 0 || n > math.MaxUint32 {
		panic(fmt.Sprintf("xref: length %d out of uint32 range", n))
	}
	return uint32(n)
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
