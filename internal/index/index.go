package index

import (
	"context"
	"fmt"
	"go/token"
	"log"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/sivchari/golance/internal/depcheck"
	"github.com/sivchari/golance/internal/depexport"
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
	// DepCAS is the machine-global, content-addressed store Build persists
	// non-root (standard-library and module-cache) dependency export data
	// into (see internal/depexport's package doc) — distinct from cas
	// above, which holds THIS repository's own root-package facts and
	// export data. nil disables persistence: Build still resolves every
	// dependency correctly (declaration-only source-checking it fresh via
	// internal/depcheck on every miss), just without ever reusing the
	// result across processes or restarts — the same degraded-but-correct
	// behavior a machine whose cache directory could not be opened falls
	// back to (see internal/server's wiring).
	DepCAS *store.CAS
	// Progress, if non-nil, is called after each package finishes
	// processing (built, skipped, or errored), with done counting up to
	// total.
	Progress func(done, total int)

	// onEvicted, if non-nil, is called after a dependency's *types.Package
	// is evicted from the shared typecheck.Cache. Test-only hook.
	onEvicted func(pkgPath string, cacheLen int)
}

// withDefaults returns a defaulted copy of *o (o itself is never mutated:
// every field is read, never assigned back through the receiver). Pointer
// receiver only to avoid an 80-byte Options copy on every call (gocritic
// hugeParam) — Go auto-addresses an addressable value like Build's own
// local opts.withDefaults() call below, so this stays callable exactly as
// before wherever the receiver is already an addressable Options value.
func (o *Options) withDefaults() Options {
	d := *o
	if d.Parallelism <= 0 {
		d.Parallelism = max(1, runtime.NumCPU()/2)
	}
	if d.BatchSize <= 0 {
		d.BatchSize = 50
	}
	if d.ToolchainFingerprint == "" {
		d.ToolchainFingerprint = runtime.Version()
	}
	return d
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
func Build(ctx context.Context, snap *graph.Snapshot, db *store.DB, cas *store.CAS, opts *Options) (Stats, error) {
	// o is Build's own private, defaulted copy of *opts (withDefaults never
	// mutates opts itself — see its own doc): every use below reads o, not
	// opts, and &o is threaded through the call chain in place of a second
	// Options parameter copy at each hop (gocritic hugeParam).
	o := opts.withDefaults()
	start := time.Now()

	fset := token.NewFileSet()
	cache := typecheck.NewCache()
	keys := newKeyTable(ctx, db)
	exp := newCASExportSource(ctx, cas, keys)
	// depMeta/depProvider resolve every non-root (stdlib/module-cache)
	// package this run's Importer needs, by declaration-only
	// source-checking it via internal/depcheck (never invoking the Go
	// toolchain's own compiler — see internal/depexport's package doc, the
	// replacement for the removed `go list -export`/graph.Snapshot.ExportFile
	// path). depExp persists each result in o.DepCAS so a dependency
	// already checked by THIS run, an earlier one, or even a different
	// repository's indexer never needs rechecking on this machine again.
	// depProvider's own LRU is sized to this run's WHOLE non-root package
	// count (depcheck.RecommendedCap), not depcheck.DefaultCap — see that
	// constant's own doc for the real, measured thrashing regression a
	// batch run like this one hits at DefaultCap's small, navigation-sized
	// capacity.
	depMeta := depcheck.NewGraphMetadataSource(snap)
	depProvider := depcheck.NewProvider(depMeta, depcheck.Options{Cap: depcheck.RecommendedCap(nonRootCount(snap))})
	depExp := depexport.NewCache(o.DepCAS, depMeta, depProvider, depexport.Options{BuildFlagsFingerprint: o.BuildFlagsFingerprint})
	imp := typecheck.NewImporter(fset, exp, depExp, cache)
	sem := semaphore.NewWeighted(int64(o.Parallelism))

	sched, total := newScheduler(snap, cache, o.onEvicted)
	if total == 0 {
		return Stats{}, nil
	}
	results := newBuildResults(db, o.BatchSize)

	var wg sync.WaitGroup
	for path := range sched.ready {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			defer sched.finish(path)

			done := runBuildJob(ctx, sem, fset, imp, exp, snap, db, cas, keys, &o, path, results)
			if o.Progress != nil {
				o.Progress(done, total)
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
		if fpErr := db.PutBuildFingerprint(o.ToolchainFingerprint); fpErr != nil {
			err = fmt.Errorf("index: record build fingerprint: %w", fpErr)
		}
	}
	return stats, err
}

// nonRootCount returns the number of non-root (stdlib/module-cache)
// packages in snap — the sizing input for depcheck.RecommendedCap (see
// Build's and Reindex's identical use).
func nonRootCount(snap *graph.Snapshot) int {
	n := 0
	for _, pkg := range snap.Packages {
		if !pkg.Root {
			n++
		}
	}
	return n
}

// runBuildJob acquires sem, processes one package via processUnit, releases
// sem, and records the outcome. It returns results.record's running total.
// A sem.Acquire failure (ctx canceled) is fatal and recorded via
// recordFatal; any error processUnit itself returns is a single package's
// own failure and never aborts the run (see buildResults.record's doc).
func runBuildJob(ctx context.Context, sem *semaphore.Weighted, fset *token.FileSet, imp *typecheck.Importer, exp *casExportSource, snap *graph.Snapshot, db *store.DB, cas *store.CAS, keys *keyTable, opts *Options, path string, results *buildResults) int {
	if err := sem.Acquire(ctx, 1); err != nil {
		return results.recordFatal(err)
	}
	outcome, skipped, typeChecked, err := processUnitRecovered(ctx, fset, imp, exp, snap, db, cas, keys, opts, path)
	sem.Release(1)
	return results.record(outcome, skipped, typeChecked, err)
}

// processUnitRecovered wraps processUnit with a panic recovery so that one
// poisoned package (e.g. a type-checker edge case facts extraction does not
// yet handle) degrades that single package into a processing error instead
// of crashing the whole indexer subprocess mid-run, taking down every other
// package's results with it. This mirrors record's existing fatal-vs-
// per-package split (see buildResults.record's doc): a recovered panic is
// reported exactly like any other single-package processUnit error, never
// as the fatal error Build itself returns. The panic value and package path
// are logged (via the standard logger, since Options carries none) because
// record itself only counts a per-package error into Stats.Errors and
// otherwise discards it — without this, a panic's cause would vanish
// entirely instead of merely being contained.
func processUnitRecovered(ctx context.Context, fset *token.FileSet, imp *typecheck.Importer, exp *casExportSource, snap *graph.Snapshot, db *store.DB, cas *store.CAS, keys *keyTable, opts *Options, path string) (outcome *unitOutcome, skipped, typeChecked bool, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("index: panic processing package %s: %v\n%s", path, rec, debug.Stack())
			outcome, skipped, typeChecked = nil, false, false
			err = fmt.Errorf("index: panic processing package %s: %v", path, rec)
		}
	}()
	// processUnit (unit.go) is unflagged and still takes Options by value —
	// the one deliberate copy at this pointer/value boundary, not repeated
	// again at each of ITS OWN further internal calls (see its own doc).
	return processUnit(ctx, fset, imp, exp, snap, db, cas, keys, *opts, path, readFileDisk, true)
}
