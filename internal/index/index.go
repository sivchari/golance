package index

import (
	"context"
	"fmt"
	"go/token"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/typecheck"
)

// Options configures a Build run.
type Options struct {
	// Parallelism bounds the number of packages type-checked concurrently.
	// Defaults to runtime.NumCPU()/2 (minimum 1).
	Parallelism int
	// BatchSize is the number of packages accumulated before a
	// store.DB.PutUnitsBatch commit. Defaults to 50.
	BatchSize int
	// ToolchainFingerprint is recorded in each package's UnitPointer and
	// compared on a later Build to force a full revalidation pass after a
	// toolchain upgrade. Defaults to runtime.Version().
	ToolchainFingerprint string
	// BuildFlagsFingerprint is folded into each package's content hash so
	// a build-flags change (e.g. -tags) invalidates the index. Optional.
	BuildFlagsFingerprint string
	// RelativePaths, if set, stores source file paths (the facts blob's
	// file table and store.UnitPointer.Files) relative to the workspace
	// root (snap.Dir()) instead of as absolute paths, so the resulting CAS
	// blobs and index are valid to open from any directory a caller
	// resolves that same root to — e.g. any git worktree of the same
	// repository (see internal/server.RelativeIndexPaths, the source of
	// truth this should always be set from). Readers (Revalidate, and
	// internal/xref.New) must be given the same value the database was
	// last written with, since two writers using different values into the
	// same database would corrupt each other's path interpretation.
	RelativePaths bool
	// Progress, if non-nil, is called after each package finishes
	// processing (built, skipped, or errored), with done counting up to
	// total.
	Progress func(done, total int)

	// onEvicted, if non-nil, is called after a dependency's *types.Package
	// is evicted from the shared typecheck.Cache. Test-only hook.
	onEvicted func(pkgPath string, cacheLen int)
}

func (o Options) withDefaults() Options {
	if o.Parallelism <= 0 {
		o.Parallelism = max(1, runtime.NumCPU()/2)
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 50
	}
	if o.ToolchainFingerprint == "" {
		o.ToolchainFingerprint = runtime.Version()
	}
	return o
}

// Stats summarizes a completed Build or Reindex run.
type Stats struct {
	Processed int
	Skipped   int
	Errors    int
	Elapsed   time.Duration
	// TypeChecked is the subset of Processed that required an actual
	// parse/type-check (checkAndStoreOutcome): a real package rebuild, as
	// opposed to a CAS hit (Processed-TypeChecked), which resolves a
	// package's export/facts data from a previously-built blob without
	// looking at its source at all. Callers that need to verify a build
	// actually avoided rechecking something (e.g. reverting to
	// previously-seen content) should assert on this directly rather than
	// on wall-clock time, which a shared CI runner's load can make an
	// unreliable proxy for the same fact.
	TypeChecked int
}

// Build resolves every root (workspace) package in snap against db and cas,
// in dependency order, type-checking only what a stat check and (see the
// package doc's key composition) a CAS lookup cannot rule out as already
// current. Processing is bounded to opts.Parallelism concurrent packages; a
// dependency's decoded *types.Package is evicted from the shared type cache
// as soon as every package that imports it has finished, keeping peak
// memory proportional to the worker count rather than to workspace size.
//
// Build's returned error is reserved for conditions that leave db
// untrustworthy as a whole: a canceled context, or a failed batch commit. A
// package that itself fails to parse or type-check does not cause an error
// here — it is only reflected in Stats.Errors — since one unbuildable
// package among many otherwise-good ones must not make an indexer exit
// non-zero and discard a mostly-successful build (see buildResults.record).
func Build(ctx context.Context, snap *graph.Snapshot, db *store.DB, cas *store.CAS, opts Options) (Stats, error) {
	opts = opts.withDefaults()
	start := time.Now()

	fset := token.NewFileSet()
	cache := typecheck.NewCache()
	keys := newKeyTable(db)
	exp := newCASExportSource(cas, keys)
	imp := typecheck.NewImporter(fset, exp, snap, cache)
	sem := semaphore.NewWeighted(int64(opts.Parallelism))

	sched, total := newScheduler(snap, cache, opts.onEvicted)
	if total == 0 {
		return Stats{}, nil
	}
	results := newBuildResults(db, opts.BatchSize)

	var wg sync.WaitGroup
	for path := range sched.ready {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			defer sched.finish(path)

			done := runBuildJob(ctx, sem, fset, imp, exp, snap, db, cas, keys, opts, path, results)
			if opts.Progress != nil {
				opts.Progress(done, total)
			}
		}(path)
	}
	wg.Wait()

	results.flush()
	stats, err := results.result()
	stats.Elapsed = time.Since(start)
	if err == nil {
		// Record the toolchain this run checked db against, so a later
		// revalidation pass (see Revalidate, used by
		// internal/server.indexNeedsRebuild) can rule out a toolchain
		// change with one cheap read instead of inspecting every package.
		// Only recorded on a run with no fatal error: a failed run's db is
		// exactly what that check must not trust.
		if fpErr := db.PutBuildFingerprint(opts.ToolchainFingerprint); fpErr != nil {
			err = fmt.Errorf("index: record build fingerprint: %w", fpErr)
		}
	}
	return stats, err
}

// runBuildJob acquires sem, processes one package via processUnit, releases
// sem, and records the outcome. It returns results.record's running total.
// A sem.Acquire failure (ctx canceled) is fatal and recorded via
// recordFatal; any error processUnit itself returns is a single package's
// own failure and never aborts the run (see buildResults.record's doc).
func runBuildJob(ctx context.Context, sem *semaphore.Weighted, fset *token.FileSet, imp *typecheck.Importer, exp *casExportSource, snap *graph.Snapshot, db *store.DB, cas *store.CAS, keys *keyTable, opts Options, path string, results *buildResults) int {
	if err := sem.Acquire(ctx, 1); err != nil {
		return results.recordFatal(err)
	}
	outcome, skipped, typeChecked, err := processUnit(fset, imp, exp, snap, db, cas, keys, opts, path, readFileDisk, true)
	sem.Release(1)
	return results.record(outcome, skipped, typeChecked, err)
}
