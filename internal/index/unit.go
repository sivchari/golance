package index

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/typecheck"
)

// parseMode is the parser mode used for every package file: full comments
// (needed for Doc extraction) without the legacy ast.Object resolution
// pass, which type-checking makes redundant.
const parseMode = parser.ParseComments | parser.SkipObjectResolution

// unitOutcome is what [processUnit] computed for one package, for the
// caller (Build's worker, or Reindex's per-hop step) to persist.
// Exactly one of entry/ptrRefresh is non-nil, or neither — the latter
// meaning nothing at all needs writing (a genuine no-op skip).
type unitOutcome struct {
	pkgHash    uint64
	entry      *store.UnitEntry   // blob key changed (CAS hit or a fresh type-check): write via PutUnitsBatch
	ptrRefresh *store.UnitPointer // key unchanged but the stat snapshot needs refreshing: write via PutUnitPointersBatch
}

// processUnit resolves path's current combined blob key against snap and
// keys, and brings db/cas up to date if it changed: a stat-only fast path
// when nothing looks touched, a CAS hit when this exact (own content,
// dependency API) combination was already built before, or a real
// parse/type-check otherwise. It is the shared core behind both Build (via
// the dependency-ordered scheduler) and Reindex (via the reverse-dependency
// closure walk) — see the package doc for the key composition that makes
// this sound for both.
//
// It always calls keys.set for path before returning (success or not, as
// long as ownHash/deps were resolved) so a dependent processed later in the
// same topologically-ordered run can find it; the one exception is a
// package with no Go files, which is never a dependency of anything (see
// directDepExports's filter) and so needs no entry.
//
// reader is used only to read goFiles' content when a content-hash
// recompute or a real type-check is needed — Reindex passes an overlay
// reader for the package the caller knows changed, and disk reads for
// everything else; Build always passes disk reads.
//
// trustStat controls whether the on-disk (size, mtime) stat fast path may
// be trusted at all: it compares against the filesystem directly
// (os.Stat), which is only meaningful when reader also reads from disk.
// Build and Reindex's closure hops both pass true (their reader is always
// readFileDisk); Reindex passes false for the package it already knows
// changed, since that call's reader may be an editor overlay whose content
// differs from disk while disk's own stat stays untouched — trusting stat
// there would silently skip a genuinely edited-but-unsaved package.
func processUnit(fset *token.FileSet, imp *typecheck.Importer, exp *casExportSource, snap *graph.Snapshot, db *store.DB, cas *store.CAS, keys *keyTable, opts Options, path string, reader FileReader, trustStat bool) (outcome *unitOutcome, skipped, typeChecked bool, err error) {
	pkg := snap.Packages[path]
	if len(pkg.GoFiles) == 0 {
		// go/packages legitimately reports root packages with no GoFiles at
		// all — e.g. a directory containing only _test.go files for an
		// external "_test" test package. There is nothing to type-check or
		// index, and nothing else can import it, so it never needs a
		// keyTable entry either.
		return nil, true, false, nil
	}
	pkgHash := store.Hash(path)
	root := snap.Dir()

	old, oldErr := db.GetUnit(pkgHash)
	haveOld := oldErr == nil
	trusted := haveOld && old.ToolchainFingerprint == opts.ToolchainFingerprint

	statOK := trustStat && trusted && len(old.Files) > 0 && filesStatMatch(pkg.GoFiles, old.Files, root, opts.RelativePaths)
	ownHash, err := resolveOwnHash(pkg, opts, reader, root, statOK, old.ContentHash)
	if err != nil {
		return nil, false, false, err
	}

	deps, err := directDepExports(snap, keys, pkg)
	if err != nil {
		return nil, false, false, err
	}
	combined := computeUnitKey(ownHash, deps)

	if trusted && combined == old.BlobKey {
		return unchangedOutcome(pkgHash, path, old, pkg, opts, root, statOK, trustStat, keys), true, false, nil
	}

	// The combined key differs from what was last recorded (or there is no
	// trusted previous pointer at all): try the CAS first. A hit needs no
	// type-check — this exact content-plus-dependency-API combination was
	// already built before, e.g. switching back to a previously-visited
	// branch (this is the common, fast-path case; see the package doc).
	if blob, ok, err := cas.Get(combined); err != nil {
		return nil, false, false, err
	} else if ok {
		outcome, err := casHitOutcome(pkgHash, path, combined, ownHash, blob, opts, exp, keys)
		if err != nil {
			return nil, false, false, err
		}
		return outcome, false, false, nil
	}

	// Miss: nobody has ever built this exact combination. Actually
	// parse/type-check it.
	outcome, err = checkAndStoreOutcome(fset, imp, cas, exp, keys, pkgHash, path, pkg, combined, ownHash, opts, reader, root, trustStat)
	if err != nil {
		return nil, false, false, err
	}
	return outcome, false, true, nil
}

// resolveOwnHash returns pkg's own content hash: the already-recorded
// old.ContentHash when statOK confirms nothing on disk has moved, or a
// fresh recompute through reader otherwise.
func resolveOwnHash(pkg *graph.Package, opts Options, reader FileReader, root string, statOK bool, oldContentHash uint64) (uint64, error) {
	if statOK {
		return oldContentHash, nil
	}
	return contentHash(pkg.GoFiles, opts.BuildFlagsFingerprint, reader, root, opts.RelativePaths)
}

// unchangedOutcome handles the case where the combined key still matches
// what db last recorded for path: nothing needs writing except possibly a
// refreshed stat snapshot, and the package is always skipped either way.
func unchangedOutcome(pkgHash uint64, path string, old store.UnitPointer, pkg *graph.Package, opts Options, root string, statOK, trustStat bool, keys *keyTable) *unitOutcome {
	keys.set(path, unitKeyRecord{blobKey: old.BlobKey, exportHash: old.ExportHash})
	if statOK {
		return nil
	}
	if !trustStat {
		// The content hash (computed through reader, possibly an editor
		// overlay) confirmed nothing changed, but disk's own stat cannot
		// be trusted here (see processUnit's trustStat doc): recording a
		// disk-based Files snapshot now, while reader's content might
		// still differ from disk, could make a later disk-trusting Build
		// wrongly skip a package whose real disk content has diverged.
		// Leave the existing pointer (and its Files) untouched; the only
		// cost is one future content-hash recheck instead of a stat-only
		// skip, never correctness.
		return nil
	}
	// The content-hash fallback confirmed nothing changed; refresh the
	// stat snapshot so a later run can skip by stat alone again.
	files, sErr := statFiles(pkg.GoFiles, root, opts.RelativePaths)
	if sErr != nil {
		return nil
	}
	refreshed := old
	refreshed.Files = files
	return &unitOutcome{pkgHash: pkgHash, ptrRefresh: &refreshed}
}

// casHitOutcome decodes a CAS hit for combined and folds it into the
// outcome processUnit returns, recording path's export in exp and its blob
// key in keys along the way.
func casHitOutcome(pkgHash uint64, path string, combined, ownHash uint64, blob []byte, opts Options, exp *casExportSource, keys *keyTable) (*unitOutcome, error) {
	u, err := store.DecodeUnitBlob(blob)
	if err != nil {
		return nil, err
	}
	exp.Put(path, u.Export)
	eh := hashExport(u.Export)
	keys.set(path, unitKeyRecord{blobKey: combined, exportHash: eh})
	pointer := store.UnitPointer{BlobKey: combined, ContentHash: ownHash, ExportHash: eh, ToolchainFingerprint: opts.ToolchainFingerprint, Files: u.Files}
	return &unitOutcome{pkgHash: pkgHash, entry: &store.UnitEntry{PkgHash: pkgHash, Pointer: pointer, Index: u.Index}}, nil
}

// checkAndStoreOutcome type-checks pkg, writes the result to cas under
// combined, and folds it into the outcome processUnit returns.
func checkAndStoreOutcome(fset *token.FileSet, imp *typecheck.Importer, cas *store.CAS, exp *casExportSource, keys *keyTable, pkgHash uint64, path string, pkg *graph.Package, combined, ownHash uint64, opts Options, reader FileReader, root string, trustStat bool) (*unitOutcome, error) {
	result, err := checkOnePackage(fset, imp, path, pkg.GoFiles, reader, root, opts.RelativePaths)
	if err != nil {
		return nil, err
	}
	// A disk-based stat snapshot is only trustworthy when reader itself
	// reads from disk (see processUnit's trustStat doc) — otherwise leave
	// Files nil, costing a future content-hash recheck instead of risking
	// a later disk-trusting Build wrongly skipping genuinely-changed
	// content.
	var files []store.FileStat
	if trustStat {
		if f, sErr := statFiles(pkg.GoFiles, root, opts.RelativePaths); sErr == nil {
			files = f
		}
	}
	if err := cas.Put(combined, store.EncodeUnitBlob(&store.UnitBlob{Facts: result.Facts, Export: result.Export, Files: files, Index: result.Index})); err != nil {
		return nil, err
	}
	exp.Put(path, result.Export)
	eh := hashExport(result.Export)
	keys.set(path, unitKeyRecord{blobKey: combined, exportHash: eh})
	pointer := store.UnitPointer{BlobKey: combined, ContentHash: ownHash, ExportHash: eh, ToolchainFingerprint: opts.ToolchainFingerprint, Files: files}
	return &unitOutcome{pkgHash: pkgHash, entry: &store.UnitEntry{PkgHash: pkgHash, Pointer: pointer, Index: result.Index}}, nil
}

// directDepExports returns pkg's direct workspace (root) dependencies'
// current export-hash contributions to its own [computeUnitKey], resolved
// through keys. Every entry is guaranteed resolvable: dependency-ordered
// processing (Build's scheduler, or Reindex's topologically-ordered closure
// walk) never asks for a dependency before it has itself been fully
// resolved this run or, if untouched, already stable in db.
func directDepExports(snap *graph.Snapshot, keys *keyTable, pkg *graph.Package) ([]depExportEntry, error) {
	var deps []depExportEntry
	for _, imp := range pkg.Imports {
		d, ok := snap.Packages[imp]
		if !ok || !d.Root || len(d.GoFiles) == 0 {
			continue // non-workspace or empty dependency: excluded from the key, see computeUnitKey's doc.
		}
		rec, ok := keys.get(imp)
		if !ok {
			return nil, fmt.Errorf("index: dependency %s of %s has no recorded blob key (processed out of order?)", imp, pkg.ImportPath)
		}
		deps = append(deps, depExportEntry{path: imp, exportHash: rec.exportHash})
	}
	return deps, nil
}

// checkResult bundles one package's freshly type-checked outputs.
type checkResult struct {
	Facts  []byte
	Export []byte
	Index  store.PackageIndexEntries
}

// checkOnePackage parses goFiles (via readFile), type-checks them as
// pkgPath using imp, and extracts facts and export data. It does not itself
// write anything: the returned checkResult is for the caller to fold into a
// [store.UnitBlob]. root and relative control whether the facts blob's file
// table stores paths relative to root (see Options.RelativePaths).
func checkOnePackage(fset *token.FileSet, imp *typecheck.Importer, pkgPath string, goFiles []string, readFile func(string) ([]byte, error), root string, relative bool) (checkResult, error) {
	files, fileList := parseGoFiles(fset, goFiles, readFile)
	if len(files) == 0 {
		return checkResult{}, fmt.Errorf("index: no parseable files for %s", pkgPath)
	}

	tpkg, info, _ := typecheck.CheckPackage(fset, files, pkgPath, imp)
	if tpkg == nil {
		return checkResult{}, fmt.Errorf("index: type-check %s produced no package", pkgPath)
	}

	pkgHash := store.Hash(pkgPath)
	b := store.NewBuilder()
	idx := extractFacts(fset, pkgHash, tpkg, info, files, fileList, b, root, relative)
	factsBlob, err := b.Build()
	if err != nil {
		return checkResult{}, fmt.Errorf("index: build facts blob for %s: %w", pkgPath, err)
	}

	exportBlob, err := typecheck.WriteExport(tpkg, fset)
	if err != nil {
		return checkResult{}, fmt.Errorf("index: write export data for %s: %w", pkgPath, err)
	}

	return checkResult{Facts: factsBlob, Export: exportBlob, Index: idx}, nil
}

// parseGoFiles parses every file in goFiles (via readFile), skipping any
// that cannot be read or produce no AST at all. It returns the parsed files
// alongside the matching subset of goFiles.
func parseGoFiles(fset *token.FileSet, goFiles []string, readFile func(string) ([]byte, error)) ([]*ast.File, []string) {
	files := make([]*ast.File, 0, len(goFiles))
	fileList := make([]string, 0, len(goFiles))
	for _, gf := range goFiles {
		src, err := readFile(gf)
		if err != nil {
			continue
		}
		f, _ := parser.ParseFile(fset, gf, src, parseMode)
		if f == nil {
			continue
		}
		files = append(files, f)
		fileList = append(fileList, gf)
	}
	return files, fileList
}
