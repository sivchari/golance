package index

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"

	"golang.org/x/sync/semaphore"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/store"
)

// Revalidate reports whether any root package in snap would need real work
// (a parse/type-check) if [Build] ran against db and its CAS right now: a
// missing or stale [store.UnitPointer], a toolchain fingerprint mismatch, or
// a genuine change to its own content or a direct dependency's exported API
// (see the package doc's key composition). relative must match the
// Options.RelativePaths value db was last built or reindexed with, so a
// stored [store.UnitPointer].Files path is joined back onto snap.Dir()
// correctly before comparison.
//
// It never writes to db, and never touches the CAS at all (every
// comparison is against already-recorded [store.UnitPointer] values, never
// a blob's own content) — so it is safe and cheap to run concurrently with
// any other use of db, including a caller that already has db open for
// interactive queries or in-session Reindex writes: bbolt permits any
// number of concurrent readers alongside one writer on the same open
// handle. Packages are checked concurrently, at a higher fan-out than
// Build's own Options.Parallelism: unlike Build, this never type-checks
// anything, so its work is I/O-bound (a stat per file, and occasionally a
// source read for the content-hash fallback) rather than CPU-bound.
func Revalidate(ctx context.Context, snap *graph.Snapshot, db *store.DB, toolchainFP, buildFlagsFP string, relative bool) (bool, error) {
	stale, wholeDBStale, err := revalidateImpl(ctx, snap, db, toolchainFP, buildFlagsFP, relative)
	if err != nil {
		return false, err
	}
	return wholeDBStale || len(stale) > 0, nil
}

// RevalidateStale is Revalidate's per-package variant: instead of a single
// bool, it returns the sorted import paths of every stale root package, for
// a caller that wants to repair only what changed — one [Reindex] call per
// entry — rather than forcing a full [Build] rebuild whenever anything at
// all is stale.
//
// wholeDBStale mirrors Revalidate's own whole-database short-circuit: db's
// build fingerprint (see [store.DB.BuildFingerprint]) is missing or does
// not match toolchainFP, so nothing recorded in db is trustworthy enough to
// compare packages against. Reindex never writes a build fingerprint (only
// [Build] calls [store.DB.PutBuildFingerprint]), so a caller must not route
// a wholeDBStale=true result through per-package Reindex — that would leave
// the fingerprint missing/mismatched forever, and every future Revalidate
// or RevalidateStale call would keep reporting the whole database stale
// regardless of how many individual packages get reindexed. When
// wholeDBStale is true, pkgs is always nil; only a full Build resolves it.
//
// Unlike a boolean short-circuit, this always scans every root package in
// snap: the caller needs the complete stale set, not merely proof that one
// exists, so there is no early exit once the first stale package is found.
func RevalidateStale(ctx context.Context, snap *graph.Snapshot, db *store.DB, toolchainFP, buildFlagsFP string, relative bool) ([]string, bool, error) {
	return revalidateImpl(ctx, snap, db, toolchainFP, buildFlagsFP, relative)
}

// PackageChanged is Revalidate's single-package variant, for a caller that
// only needs a cheap answer for one specific root package — e.g.
// internal/server's didOpen-time self-heal — rather than fanning out over
// the whole workspace. It costs one [store.DB.BuildFingerprint] read, one
// [store.DB.GetUnit] read, and stat-based hashing of path's own files only;
// it never touches any other package's files.
//
// The whole-database build-fingerprint check runs first, exactly as in
// Revalidate: if it is missing or does not match toolchainFP, PackageChanged
// reports true without even looking path up in snap, since nothing recorded
// in db could be trusted regardless of what path's own state turns out to
// be.
//
// path must name a root package known to snap (see
// [graph.Snapshot.Package]); an unknown path is a caller bug — asking about
// a package that does not exist in the current workspace graph — and is
// reported as an error rather than as "changed". A path that snap does know
// about but db has never recorded a [store.UnitPointer] for (e.g. a
// newly-added package) does report true, via the same path packageChanged
// already takes for that case.
func PackageChanged(ctx context.Context, snap *graph.Snapshot, db *store.DB, path, toolchainFP, buildFlagsFP string, relative bool) (bool, error) {
	fp, err := db.BuildFingerprint()
	notFound := errors.Is(err, store.ErrNotFound)
	if err != nil && !notFound {
		return false, fmt.Errorf("index: packagechanged: read build fingerprint: %w", err)
	}
	if notFound || fp != toolchainFP {
		return true, nil
	}

	pkg, ok := snap.Package(path)
	if !ok {
		return false, fmt.Errorf("index: packagechanged: unknown package %s", path)
	}

	keys := newKeyTable(ctx, db)
	return packageChanged(ctx, db, keys, snap, pkg, path, toolchainFP, buildFlagsFP, snap.Dir(), relative)
}

// revalidateImpl is the shared implementation behind Revalidate and
// RevalidateStale: it scans every root package in snap concurrently and
// reports which ones are stale (see packageChanged), short-circuiting only
// on the cheap whole-database build-fingerprint check both public entry
// points document.
func revalidateImpl(ctx context.Context, snap *graph.Snapshot, db *store.DB, toolchainFP, buildFlagsFP string, relative bool) (stale []string, wholeDBStale bool, err error) {
	// Cheap whole-database short-circuit: if db was never fully built under
	// the running toolchain at all (see index.Build's PutBuildFingerprint),
	// every package needs rechecking, so there is no point fanning out a
	// per-package comparison that would find exactly that. A genuine read
	// error (as opposed to store.ErrNotFound, the expected not-yet-built
	// state) is not the same thing and must propagate, so the caller's own
	// conservative fallback (keep serving what is already open rather than
	// force a rebuild on a transient error — see
	// internal/server.indexNeedsRebuild) applies instead of masking the
	// error as "everything changed."
	fp, err := db.BuildFingerprint()
	notFound := errors.Is(err, store.ErrNotFound)
	if err != nil && !notFound {
		return nil, false, fmt.Errorf("index: revalidate: read build fingerprint: %w", err)
	}
	// notFound and a genuine fingerprint mismatch both mean the same thing
	// here: nothing trustworthy to compare packages against, so report the
	// whole database stale without a per-package fan-out.
	if notFound || fp != toolchainFP {
		return nil, true, nil
	}

	root := snap.Dir()
	keys := newKeyTable(ctx, db)
	sem := semaphore.NewWeighted(int64(max(1, runtime.NumCPU()*2)))
	var mu sync.Mutex
	var staleList []string
	var firstErr firstErrRecorder
	var wg sync.WaitGroup
	for path, pkg := range snap.Packages {
		if !pkg.Root || len(pkg.GoFiles) == 0 {
			continue
		}
		wg.Add(1)
		go func(path string, pkg *graph.Package) {
			defer wg.Done()
			if err := sem.Acquire(ctx, 1); err != nil {
				firstErr.record(err)
				return
			}
			defer sem.Release(1)

			pkgChanged, err := packageChanged(ctx, db, keys, snap, pkg, path, toolchainFP, buildFlagsFP, root, relative)
			if err != nil {
				firstErr.record(err)
				return
			}
			if pkgChanged {
				mu.Lock()
				staleList = append(staleList, path)
				mu.Unlock()
			}
		}(path, pkg)
	}
	wg.Wait()
	if err := firstErr.get(); err != nil {
		return nil, false, err
	}
	sort.Strings(staleList)
	return staleList, false, nil
}

// packageChanged reports whether pkg differs from what db last recorded for
// it, without writing anything: a missing pointer, a toolchain fingerprint
// mismatch, a dependency that has never been indexed at all, or — the
// common case — a recomputed combined key ([computeUnitKey]) that no longer
// matches the stored [store.UnitPointer].BlobKey.
func packageChanged(ctx context.Context, db *store.DB, keys *keyTable, snap *graph.Snapshot, pkg *graph.Package, path, toolchainFP, buildFlagsFP, root string, relative bool) (bool, error) {
	old, err := db.GetUnit(ctx, store.Hash(path))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return true, nil
		}
		return false, fmt.Errorf("index: revalidate: get unit for %s: %w", path, err)
	}
	if old.ToolchainFingerprint != toolchainFP {
		return true, nil
	}

	// effectiveFiles must mirror processUnit's own file set exactly (see
	// testFilesInPackage's doc): otherwise a stat/content-hash comparison
	// against pkg.GoFiles alone would never match what Build last recorded
	// for a package with in-package test files (its stored Files/
	// ContentHash always cover them too), making Revalidate report every
	// such package as changed on every call regardless of whether anything
	// actually did. Always disk-based (readFileDisk): unlike Reindex,
	// Revalidate never runs against an editor overlay.
	testFiles := testFilesInPackage(pkg, readFileDisk)
	effectiveFiles := effectiveGoFiles(pkg.GoFiles, testFiles)

	var ownHash uint64
	if len(old.Files) > 0 && filesStatMatch(effectiveFiles, old.Files, root, relative) {
		ownHash = old.ContentHash
	} else {
		h, err := contentHash(effectiveFiles, buildFlagsFP, readFileDisk, root, relative)
		if err != nil {
			return false, err
		}
		ownHash = h
	}

	var deps []depExportEntry
	for _, imp := range pkg.Imports {
		d, ok := snap.Packages[imp]
		if !ok || !d.Root || len(d.GoFiles) == 0 {
			continue
		}
		rec, ok := keys.get(imp)
		if !ok {
			return true, nil // a dependency that has never been indexed at all: conservatively report changed.
		}
		deps = append(deps, depExportEntry{path: imp, exportHash: rec.exportHash})
	}

	return computeUnitKey(ownHash, deps) != old.BlobKey, nil
}

// firstErrRecorder keeps the first non-nil error reported to it by any
// number of concurrent goroutines.
type firstErrRecorder struct {
	mu  sync.Mutex
	err error
}

func (r *firstErrRecorder) record(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err == nil {
		r.err = err
	}
}

func (r *firstErrRecorder) get() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}
