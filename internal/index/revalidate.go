package index

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"

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
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, fmt.Errorf("index: revalidate: read build fingerprint: %w", err)
	}
	if err != nil || fp != toolchainFP {
		return true, nil
	}

	root := snap.Dir()
	keys := newKeyTable(db)
	sem := semaphore.NewWeighted(int64(max(1, runtime.NumCPU()*2)))
	var changed atomic.Bool
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

			pkgChanged, err := packageChanged(db, keys, snap, pkg, path, toolchainFP, buildFlagsFP, root, relative)
			if err != nil {
				firstErr.record(err)
				return
			}
			if pkgChanged {
				changed.Store(true)
			}
		}(path, pkg)
	}
	wg.Wait()
	if err := firstErr.get(); err != nil {
		return false, err
	}
	return changed.Load(), nil
}

// packageChanged reports whether pkg differs from what db last recorded for
// it, without writing anything: a missing pointer, a toolchain fingerprint
// mismatch, a dependency that has never been indexed at all, or — the
// common case — a recomputed combined key ([computeUnitKey]) that no longer
// matches the stored [store.UnitPointer].BlobKey.
func packageChanged(db *store.DB, keys *keyTable, snap *graph.Snapshot, pkg *graph.Package, path, toolchainFP, buildFlagsFP, root string, relative bool) (bool, error) {
	old, err := db.GetUnit(store.Hash(path))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return true, nil
		}
		return false, fmt.Errorf("index: revalidate: get unit for %s: %w", path, err)
	}
	if old.ToolchainFingerprint != toolchainFP {
		return true, nil
	}

	var ownHash uint64
	if len(old.Files) > 0 && filesStatMatch(pkg.GoFiles, old.Files, root, relative) {
		ownHash = old.ContentHash
	} else {
		h, err := contentHash(pkg.GoFiles, buildFlagsFP, readFileDisk, root, relative)
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
