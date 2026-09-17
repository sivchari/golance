package index

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"log"
	"runtime/debug"

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
	incomplete bool               // a fresh type-check produced this entry with real errors (see checkResult.Incomplete); never set for a CAS hit or a stat-only refresh
	firstError string             // sample error for the Incomplete log line (see checkResult.FirstError)
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
// The stat-only fast path additionally requires cas.Has(old.BlobKey): a
// recorded key matching what db already has proves db's own bookkeeping is
// self-consistent, never that the blob it names still exists in cas — a
// [store.CAS.GC] pass whose mark set could not read this database in time
// (see its own safety doc) can sweep it out from under an otherwise
// untouched package. Without this check that package would report
// "unchanged" forever while every read of it failed, since nothing else
// ever re-examines a key that already matches. The check costs one extra
// os.Stat per package on an otherwise stat-only path (see [store.CAS.Has]);
// that is deliberately not cached here — see the package doc if a
// benchmark ever shows it matters enough to.
//
// It calls keys.set for path on success (see keyTable's own doc) so a
// dependent processed later in the same topologically-ordered run can find
// it; the one exception is a package with no Go files, which is never a
// dependency of anything (see directDepExports's filter) and so needs no
// entry either way. A non-nil err here never calls keys.set — this
// function's sole caller, processUnitRecovered, is instead responsible for
// calling keys.fail for path in that case (see its own doc), so the
// resulting "exactly one of set or fail" invariant holds regardless of
// which of processUnit's several error-return sites is hit, without each
// of them having to remember to do it individually.
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
func processUnit(ctx context.Context, fset *token.FileSet, imp *typecheck.Importer, exp *casExportSource, snap *graph.Snapshot, db *store.DB, cas *store.CAS, keys *keyTable, opts *Options, path string, reader FileReader, trustStat bool) (outcome *unitOutcome, skipped, typeChecked bool, err error) {
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

	// testFiles are pkg's in-package _test.go files (see
	// testFilesInPackage's doc); effectiveFiles is the full set every
	// stat/content-hash comparison and, ultimately, checkOnePackage's facts
	// pass below indexes for this unit. checkOnePackage still receives
	// pkg.GoFiles and testFiles separately: its own export-data pass must
	// stay derived from pkg.GoFiles alone (see its doc).
	testFiles := testFilesInPackage(pkg, reader)
	effectiveFiles := effectiveGoFiles(pkg.GoFiles, testFiles)

	old, oldErr := db.GetUnit(ctx, pkgHash)
	haveOld := oldErr == nil
	trusted := haveOld && old.ToolchainFingerprint == opts.ToolchainFingerprint

	statOK := trustStat && trusted && len(old.Files) > 0 && filesStatMatch(effectiveFiles, old.Files, root, opts.RelativePaths)
	ownHash, err := resolveOwnHash(effectiveFiles, opts, reader, root, statOK, old.ContentHash)
	if err != nil {
		return nil, false, false, err
	}

	deps, err := directDepExports(snap, keys, pkg)
	if err != nil {
		return nil, false, false, err
	}
	combined := computeUnitKey(ownHash, deps)

	if trusted && combined == old.BlobKey && cas.Has(old.BlobKey) {
		return unchangedOutcome(pkgHash, path, old, effectiveFiles, opts, root, statOK, trustStat, keys), true, false, nil
	}

	// The combined key differs from what was last recorded, there is no
	// trusted previous pointer at all, or old.BlobKey matched but its own
	// blob is gone from cas (see the cas.Has check above): try the CAS
	// first. A hit needs no type-check — this exact content-plus-
	// dependency-API combination was already built before, e.g. switching
	// back to a previously-visited branch (this is the common, fast-path
	// case; see the package doc) — except in the dangling-pointer case,
	// where combined == old.BlobKey but the blob itself is missing, so this
	// is always a miss there and falls through to a genuine re-type-check
	// below, rewriting the same key's blob.
	if blob, ok, err := cas.Get(ctx, combined); err != nil {
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
	outcome, err = checkAndStoreOutcome(fset, imp, cas, exp, keys, pkgHash, path, pkg, testFiles, combined, ownHash, opts, reader, root, trustStat)
	if err != nil {
		return nil, false, false, err
	}
	return outcome, false, true, nil
}

// resolveOwnHash returns pkg's own content hash: the already-recorded
// old.ContentHash when statOK confirms nothing on disk has moved, or a
// fresh recompute through reader otherwise. goFiles is the unit's full
// effective file set (see effectiveGoFiles).
func resolveOwnHash(goFiles []string, opts *Options, reader FileReader, root string, statOK bool, oldContentHash uint64) (uint64, error) {
	if statOK {
		return oldContentHash, nil
	}
	return contentHash(goFiles, opts.BuildFlagsFingerprint, reader, root, opts.RelativePaths)
}

// unchangedOutcome handles the case where the combined key still matches
// what db last recorded for path: nothing needs writing except possibly a
// refreshed stat snapshot, and the package is always skipped either way.
// effectiveFiles is the unit's full effective file set (see
// effectiveGoFiles).
func unchangedOutcome(pkgHash uint64, path string, old store.UnitPointer, effectiveFiles []string, opts *Options, root string, statOK, trustStat bool, keys *keyTable) *unitOutcome {
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
	files, sErr := statFiles(effectiveFiles, root, opts.RelativePaths)
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
func casHitOutcome(pkgHash uint64, path string, combined, ownHash uint64, blob []byte, opts *Options, exp *casExportSource, keys *keyTable) (*unitOutcome, error) {
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

// checkAndStoreOutcome type-checks pkg (plus testFiles, pkg's in-package
// _test.go files — see checkOnePackage's doc), writes the result to cas
// under combined, and folds it into the outcome processUnit returns.
func checkAndStoreOutcome(fset *token.FileSet, imp *typecheck.Importer, cas *store.CAS, exp *casExportSource, keys *keyTable, pkgHash uint64, path string, pkg *graph.Package, testFiles []string, combined, ownHash uint64, opts *Options, reader FileReader, root string, trustStat bool) (*unitOutcome, error) {
	result, err := checkOnePackage(fset, imp, path, pkg.GoFiles, testFiles, reader, root, opts.RelativePaths)
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
		if f, sErr := statFiles(effectiveGoFiles(pkg.GoFiles, testFiles), root, opts.RelativePaths); sErr == nil {
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
	return &unitOutcome{pkgHash: pkgHash, entry: &store.UnitEntry{PkgHash: pkgHash, Pointer: pointer, Index: result.Index}, incomplete: result.Incomplete, firstError: result.FirstError}, nil
}

// directDepExports returns pkg's direct workspace (root) dependencies'
// current export-hash contributions to its own [computeUnitKey], resolved
// through keys — walking both pkg.Imports and pkg.TestImports (an
// in-package test file's own extra imports, disjoint from Imports by
// construction — see graph.Package.TestImports's doc), so a package whose
// only edge to a dependency is through its own _test.go file still gets a
// combined key that changes when that dependency's export data does; this
// is what actually makes [graph.Snapshot.ClosureUnits] walking it worth
// doing (see Reindex's closure walk) rather than every visit resolving to
// an unchanged no-op. Dependency-ordered processing (Build's scheduler, or
// Reindex's topologically-ordered closure walk) never asks for a dependency
// before it has itself finished this run — resolved and stable in db (if
// left untouched), freshly resolved (if touched), or, if it was touched but
// failed to resolve, explicitly recorded as such via keys.fail (see
// keyTable's own doc for why get must not fall back to db in that case).
// directDepImports already excludes any edge not itself covered by that
// guarantee (see its own doc), so the only way keys.get still reports a
// direct dependency unresolvable here is the failed-this-run case, and pkg
// must then also be reported as this run's own error rather than silently
// keyed against that dependency's stale prior state.
func directDepExports(snap *graph.Snapshot, keys *keyTable, pkg *graph.Package) ([]depExportEntry, error) {
	var deps []depExportEntry
	for _, imp := range directDepImports(snap, pkg) {
		rec, ok := keys.get(imp)
		if !ok {
			if failErr := keys.failure(imp); failErr != nil {
				return nil, fmt.Errorf("index: dependency %s of %s failed to resolve this run: %w", imp, pkg.ImportPath, failErr)
			}
			return nil, fmt.Errorf("index: dependency %s of %s has no recorded blob key (processed out of order?)", imp, pkg.ImportPath)
		}
		deps = append(deps, depExportEntry{path: imp, exportHash: rec.exportHash})
	}
	return deps, nil
}

// directDepImports returns the import paths of pkg's direct workspace
// (root, non-empty) dependencies, folding in both pkg.Imports and
// pkg.TestImports (an in-package test file's own extra imports, disjoint
// from Imports by construction — see graph.Package.TestImports's doc) —
// the shared dependency set directDepExports and revalidate.go's
// packageChanged both need for their own key/staleness computation to
// recognize a dependency pkg reaches only through its own _test.go file
// (see graph.Snapshot.ClosureUnits' identical fold, the reverse direction
// of this same relationship).
//
// An edge snap.Before(imp, pkg.ImportPath) does not confirm — a TestImports
// edge caught in the rare legal test-only import cycle topoOrder's own
// fallback could not fully order (see its doc), e.g. two layers whose
// in-package tests import each other (a domain-usecase/infrastructure-style
// split) — is excluded here too, mirroring exactly which edges
// schedulableDepsOf treats as safe to wait on for scheduling. Without this,
// directDepExports could ask keys.get for a dependency the scheduler never
// guaranteed would be processed first, surfacing as a spurious "has no
// recorded blob key (processed out of order?)" error for both sides of the
// cycle even though neither package's own indexing is actually broken (see
// the package's fix history for the bug this closed). The cost is narrow
// and identical in kind to the scheduler's own: that one excluded
// dependency's export data never contributes to pkg's key, so an API change
// on the far side of such a cycle edge alone will not force pkg's
// reprocessing — a soundness gap already accepted for scheduling, now
// shared by key computation instead of contradicting it.
func directDepImports(snap *graph.Snapshot, pkg *graph.Package) []string {
	var out []string
	for _, imports := range [][]string{pkg.Imports, pkg.TestImports} {
		for _, imp := range imports {
			d, ok := snap.Packages[imp]
			if !ok || !d.Root || len(d.GoFiles) == 0 {
				continue // non-workspace or empty dependency: excluded from the key, see computeUnitKey's doc.
			}
			if !snap.Before(imp, pkg.ImportPath) {
				continue // see this function's own doc: an edge the scheduler itself would not wait on.
			}
			out = append(out, imp)
		}
	}
	return out
}

// checkResult bundles one package's freshly type-checked outputs.
type checkResult struct {
	Facts  []byte
	Export []byte
	Index  store.PackageIndexEntries
	// Incomplete reports whether either type-check pass below (the
	// export-producing one, or the facts one when testFiles is non-empty)
	// reported at least one go/types error -- see checkOnePackage's own
	// doc for why an error here does not fail the whole package outright,
	// and Stats.Incomplete's doc for what an Incomplete package's facts
	// can and cannot be trusted for.
	Incomplete bool
	// FirstError is the first go/types error either pass reported when
	// Incomplete is true — a diagnostic sample for the per-package log line,
	// since the full error list is too noisy to persist.
	FirstError string
}

// checkOnePackage parses goFiles (via readFile), type-checks them as
// pkgPath using imp, and extracts export data from that result exactly as
// before. If testFiles is non-empty (pkg's in-package _test.go files, see
// testFilesInPackage), it also runs a second, test-inclusive type-check —
// goFiles plus testFiles — and extracts facts from that result instead, so
// a test file's own definitions and its usages of the rest of the package
// are indexed too (mirroring internal/check.Engine, which has folded these
// in since v0.1.7 for hover/completion/inlay hints). When testFiles is
// empty this is exactly the single type-check checkOnePackage has always
// done; the extra pass only runs for packages that actually declare
// in-package test files.
//
// Export data is always derived from goFiles alone, deliberately never from
// the test-inclusive pass: a downstream importer never sees a package's
// test files (nothing ever imports them), so folding them into the
// type-check that produces Export would both leak test-only symbols into
// what importers see and change [store.UnitPointer].ExportHash on every
// test-only edit, forcing needless dependent rebuilds (see
// computeUnitKey's doc for why ExportHash soundness matters). This is the
// same test/non-test "variant" split gopls itself draws, applied minimally
// here: only the facts pass ever sees testFiles.
//
// checkOnePackage does not itself write anything: the returned checkResult
// is for the caller to fold into a [store.UnitBlob]. root and relative
// control whether the facts blob's file table stores paths relative to root
// (see Options.RelativePaths).
//
// Every error [typecheck.CheckPackage] reports for either pass is folded
// into checkResult.Incomplete rather than failing pkgPath outright: a
// dependency resolved through this run's Importer can itself be wrong for
// reasons outside pkgPath's own source (see internal/depcheck's package doc
// on why a non-root dependency is only ever declaration-checked, never
// compiler-verified), and go/types' own error recovery continues checking
// the rest of the file regardless -- so a handful of real errors, however
// their root cause, degrades to a still-mostly-usable facts/export pair
// rather than losing the package's index entry entirely. The cost of that
// leniency is real, though: go/types' recovery from an error can leave
// SPECIFIC expressions elsewhere in the very same file unresolved (no
// info.Uses entry at all, not merely a Typ[Invalid] one) — silently
// dropping the [store.Ref]s extractFacts would otherwise have recorded for
// them, with no per-position signal distinguishing "genuinely never
// referenced" from "this one specific reference was lost to error
// recovery." Incomplete is the closest signal available short of a full
// store-schema change to record which positions those were.
func checkOnePackage(fset *token.FileSet, imp *typecheck.Importer, pkgPath string, goFiles, testFiles []string, readFile func(string) ([]byte, error), root string, relative bool) (checkResult, error) {
	files, fileList, cgoSkipped := parseGoFiles(fset, goFiles, readFile)
	if len(files) == 0 {
		if cgoSkipped > 0 {
			// Every one of pkgPath's Go files imported "C" (see
			// importsCgo's own doc): there is nothing left to
			// declaration-check or index, but pkgPath still has real
			// GoFiles as far as the workspace graph is concerned, so
			// another package can still legally import it. Degrade to a
			// clean Incomplete result — still keyed via directDepExports'
			// own keys.set for any such dependent to find — rather than
			// the error below, which would leave pkgPath with no index
			// entry at all and cascade "has no recorded blob key" failures
			// to every package that imports it. An empty-but-valid facts
			// blob (store.NewView requires at least a header — nil is not
			// a valid substitute) records zero symbols/refs/files.
			emptyFacts, err := store.NewBuilder().Build()
			if err != nil {
				return checkResult{}, fmt.Errorf("index: build empty facts blob for %s: %w", pkgPath, err)
			}
			return checkResult{
				Facts:      emptyFacts,
				Incomplete: true,
				FirstError: fmt.Sprintf("index: %s has only cgo file(s) (%d of %d skipped); nothing to declaration-check or index", pkgPath, cgoSkipped, len(goFiles)),
			}, nil
		}
		return checkResult{}, fmt.Errorf("index: no parseable files for %s", pkgPath)
	}

	// scope pins, for both passes below, every dependency this call's own
	// import resolution touches — directly or transitively via another
	// already-decoded package's export data — against imp's shared Cache
	// being evicted mid-check by a concurrently-finishing, unrelated
	// package's own scheduler.finish (see CheckScope's own doc).
	scope := imp.NewCheck()
	defer scope.Close()

	tpkg, info, errs := typecheck.CheckPackage(fset, files, pkgPath, scope)
	if tpkg == nil {
		return checkResult{}, fmt.Errorf("index: type-check %s produced no package", pkgPath)
	}

	factsTpkg, factsInfo := tpkg, info
	factsFiles, factsFileList := files, fileList
	incomplete := len(errs) > 0
	firstError := ""
	if incomplete {
		firstError = errs[0].Error()
	}

	// exportBlob is attempted from tpkg regardless of errs: go/types' error
	// recovery usually leaves the exported API's own declarations perfectly
	// usable even when SOME error was reported elsewhere in the file (an
	// unrelated import failure, a generic-interface-satisfaction mismatch in
	// a declaration nothing exported references, ...) — withholding on
	// errs>0 unconditionally starves every dependent of an export that would
	// have decoded fine, which is worse than the leniency this whole
	// function already extends to Incomplete (see its own doc). Only a blob
	// that genuinely fails writeAndValidateExport's own round-trip check —
	// tpkg reaching gcexportdata.Write with an object taint that check
	// errors alone do not reliably predict, see its doc — is withheld: a
	// dependent asking for THIS pkgPath's export data then gets
	// casExportSource's own clean miss (see its ExportData doc — an empty
	// blob is never reported as a hit), so Importer.resolve falls through to
	// its fallback tier (internal/depexport.Cache, declaration-only
	// source-checking pkgPath itself — see depcheck.GraphMetadataSource's
	// doc for why this works for a root package too) instead of decoding
	// corrupt bytes or failing outright on an empty ones.
	exportBlob, roundTripErr := writeAndValidateExport(tpkg, fset)
	if roundTripErr != nil {
		exportBlob = nil
		incomplete = true
		firstError = roundTripErr.Error()
	}
	if len(testFiles) > 0 {
		testASTs, testFileList, _ := parseGoFiles(fset, testFiles, readFile)
		factsFiles = append(append([]*ast.File(nil), files...), testASTs...)
		factsFileList = append(append([]string(nil), fileList...), testFileList...)
		ftpkg, finfo, testErrs := typecheck.CheckPackage(fset, factsFiles, pkgPath, scope)
		if ftpkg == nil {
			return checkResult{}, fmt.Errorf("index: type-check %s (with test files) produced no package", pkgPath)
		}
		factsTpkg, factsInfo = ftpkg, finfo
		if firstError == "" && len(testErrs) > 0 {
			firstError = testErrs[0].Error()
		}
		incomplete = incomplete || len(testErrs) > 0
	}

	pkgHash := store.Hash(pkgPath)
	b := store.NewBuilder()
	idx := extractFacts(fset, pkgHash, factsTpkg, factsInfo, factsFiles, factsFileList, b, root, relative)
	factsBlob, err := b.Build()
	if err != nil {
		return checkResult{}, fmt.Errorf("index: build facts blob for %s: %w", pkgPath, err)
	}

	return checkResult{Facts: factsBlob, Export: exportBlob, Index: idx, Incomplete: incomplete, FirstError: firstError}, nil
}

// writeAndValidateExport first refuses tpkg outright if
// typecheck.DuplicateImportPath finds it — the confirmed cause of every
// production round-trip-decode failure so far (see its own doc for the
// x/tools decode-site) — then encodes tpkg's exported API and immediately
// decodes it back (into a throwaway fset/cache, never one a real caller
// shares — mirroring internal/depexport.Cache.checkAndPersist's identical
// self-check) before trusting the result, so a write that silently produced
// bytes gcexportdata.Read cannot reliably decode is caught here instead of
// persisted to cas and served to every dependent that imports pkgPath.
//
// The round-trip check remains the backstop for every OTHER corruption
// shape, including this one distinct from (but related to)
// the identity-split family typecheck.CheckScope's own doc describes:
// go/types.Config.Check's error recovery can leave a declaration reachable
// from tpkg's own exported API — most concretely, a generic instantiation's
// synthesized method, e.g. when a call site's own generic-interface-
// satisfaction check fails — with an invalid or missing *types.Signature.
// gcexportdata's writer (internal/gcimporter's non-shallow doDecl) has no
// guard for that: confirmed reproducible, it can panic
// (go/types.(*Signature).Recv on a nil receiver) or otherwise produce a
// blob that writes without error but panics ("internal error while
// importing ...: invalid memory address or nil pointer dereference",
// gcimporter's own recovered-panic error shape) on a later decode. The
// caller calls this unconditionally, even when tpkg's own check reported
// errors (see checkOnePackage's own doc for why withholding on errs>0
// alone is too strict): a check error does not reliably predict this
// panic, and the vast majority of error-tainted checks still produce a
// perfectly decodable blob, so this round-trip is what actually decides
// whether tpkg's export is safe to use — not whether its check was clean.
//
// WriteExport's own panic is recovered here, not left to
// processUnitRecovered's much coarser recover: that one discards pkgPath's
// entry entirely (facts included), where this call's own caller degrades to
// Incomplete instead — the facts extractFacts produces from tpkg/info never
// go through gcexportdata at all, so they are unaffected by whatever made
// Write panic and remain safe to keep.
func writeAndValidateExport(tpkg *types.Package, fset *token.FileSet) (blob []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			// The stack is logged, not folded into err, the same split
			// processUnitRecovered's own top-level recover uses: firstError
			// (this error's eventual destination — see checkOnePackage's
			// doc) is a one-line diagnostic sample, not a dump target, but
			// WriteExport's panic site is otherwise unrecoverable
			// information once this defer returns.
			log.Printf("index: write export data for %s panicked: %v\n%s", tpkg.Path(), r, debug.Stack())
			err = fmt.Errorf("index: write export data for %s panicked: %v", tpkg.Path(), r)
		}
	}()
	if dup := typecheck.DuplicateImportPath(tpkg); dup != "" {
		return nil, fmt.Errorf("index: export data for %s would reference two non-identical packages both named %q (an identity split somewhere in its dependency resolution — see typecheck.DuplicateImportPath's doc)", tpkg.Path(), dup)
	}
	blob, err = typecheck.WriteExport(tpkg, fset)
	if err != nil {
		return nil, fmt.Errorf("index: write export data for %s: %w", tpkg.Path(), err)
	}
	if _, err := typecheck.ReadExport(blob, token.NewFileSet(), tpkg.Path(), typecheck.NewCache()); err != nil {
		return nil, fmt.Errorf("index: export data for %s does not round-trip decode: %w", tpkg.Path(), err)
	}
	return blob, nil
}

// parseGoFiles parses every file in goFiles (via readFile), skipping any
// that cannot be read or produce no AST at all, and any that imports the
// pseudo-package "C" (see importsCgo's own doc). It returns the parsed
// files alongside the matching subset of goFiles, plus how many files were
// skipped specifically for being cgo — checkOnePackage's own zero-files
// handling needs that count to tell "every file was cgo" apart from
// "nothing could be read/parsed at all" (see its doc).
func parseGoFiles(fset *token.FileSet, goFiles []string, readFile func(string) ([]byte, error)) (files []*ast.File, fileList []string, cgoSkipped int) {
	files = make([]*ast.File, 0, len(goFiles))
	fileList = make([]string, 0, len(goFiles))
	for _, gf := range goFiles {
		src, err := readFile(gf)
		if err != nil {
			continue
		}
		f, _ := parser.ParseFile(fset, gf, src, parseMode)
		if f == nil {
			continue
		}
		if importsCgo(f) {
			cgoSkipped++
			continue
		}
		files = append(files, f)
		fileList = append(fileList, gf)
	}
	return files, fileList, cgoSkipped
}

// importsCgo reports whether f imports the pseudo-package "C" — the marker
// of a cgo file, which a checker with no cgo preprocessing must skip
// entirely rather than feed go/types a broken "C" reference: "C" resolves
// to nothing the workspace's import graph can name, and the file's
// declarations lean on cgo preprocessing this indexer never runs.
// Duplicated from internal/depcheck's identical helper (its own doc has
// the full rationale, including the observed net/cgo_linux.go case) rather
// than exported and imported across packages for one six-line predicate.
func importsCgo(f *ast.File) bool {
	for _, imp := range f.Imports {
		if imp.Path != nil && imp.Path.Value == `"C"` {
			return true
		}
	}
	return false
}
