package index

import (
	"errors"
	"log"
	"sync"

	"github.com/sivchari/golance/internal/store"
)

// buildResults accumulates a Build run's outcome: Stats counters, batched
// UnitEntry and pointer-only-refresh writes (see [unitOutcome]), and any
// errors encountered, all under one mutex shared by every worker.
type buildResults struct {
	mu          sync.Mutex
	db          *store.DB
	batchSize   int
	pending     []store.UnitEntry
	pendingPtrs map[uint64]store.UnitPointer
	stats       Stats
	err         error
}

func newBuildResults(db *store.DB, batchSize int) *buildResults {
	return &buildResults{db: db, batchSize: batchSize}
}

// record applies one package's outcome: exactly one of outcome/skipped/err
// describes what happened. It returns the running total of packages
// accounted for, for Options.Progress.
//
// A non-nil err here is a single package's own parse/type-check/facts
// failure: it only counts toward Stats.Errors, never toward the error
// Build itself returns. Build's contract is that its returned error is
// reserved for conditions that make the whole run's output untrustworthy
// (see recordFatal); a handful of unbuildable packages among thousands of
// good ones is not one of them, and must not turn a successful index
// build into a reported failure. err is still logged, naming path, via the
// standard logger — the same mechanism processUnitRecovered's panic
// recovery already uses (see its doc) — since Stats.Errors alone cannot
// say which package failed or why, and the caller (cmd/golance's indexer
// subprocess) exits 0 for this case, so nothing else ever surfaces it.
func (r *buildResults) record(path string, outcome *unitOutcome, skipped, typeChecked bool, err error) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch {
	case err != nil:
		r.stats.Errors++
		log.Printf("index: failed to process package %s: %v", path, err)
	case skipped:
		r.stats.Skipped++
	default:
		r.stats.Processed++
		if typeChecked {
			r.stats.TypeChecked++
		}
		if outcome != nil && outcome.incomplete {
			r.stats.Incomplete++
			log.Printf("index: %s type-checked with errors; its facts may be missing some references (see Stats.Incomplete); first: %s", path, outcome.firstError)
		}
	}
	r.queueOutcomeLocked(outcome)
	return r.stats.Processed + r.stats.Skipped + r.stats.Errors
}

// queueOutcomeLocked queues outcome's entry and/or pointer-only refresh for
// the next batch flush, flushing early once either batch reaches
// r.batchSize. outcome may be nil (a skip or a package-level error, neither
// of which produces anything to write).
func (r *buildResults) queueOutcomeLocked(outcome *unitOutcome) {
	if outcome == nil {
		return
	}
	if outcome.entry != nil {
		r.pending = append(r.pending, *outcome.entry)
		if len(r.pending) >= r.batchSize {
			r.flushPendingLocked()
		}
	}
	if outcome.ptrRefresh != nil {
		if r.pendingPtrs == nil {
			r.pendingPtrs = make(map[uint64]store.UnitPointer, r.batchSize)
		}
		r.pendingPtrs[outcome.pkgHash] = *outcome.ptrRefresh
		if len(r.pendingPtrs) >= r.batchSize {
			r.flushPtrsLocked()
		}
	}
}

// recordFatal records a systemic failure — e.g. sem.Acquire returning
// because ctx was canceled — that aborts the run rather than describing
// one package. Unlike record, this sets the error Build returns: the
// resulting Stats can no longer be trusted as a complete accounting of
// every package.
func (r *buildResults) recordFatal(err error) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stats.Errors++
	r.addErrLocked(err)
	return r.stats.Processed + r.stats.Skipped + r.stats.Errors
}

// flush commits any pending batch immediately, without waiting for
// batchSize to be reached.
func (r *buildResults) flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushPendingLocked()
	r.flushPtrsLocked()
}

// flushPendingLocked commits any pending UnitEntry batch. A commit failure
// is reported the same way a single package's own processing failure is
// (see record) — counted into Stats.Errors and logged, but never turned
// into Build's own fatal error (r.err, see recordFatal) — rather than
// discarding the whole run's trustworthiness: PutUnitsBatch commits batch
// as one bbolt transaction, so a failure here can only mean these up-to-
// batchSize packages were never persisted, not that any earlier batch
// (already durably committed) was rolled back or that db's toolchain-level
// identity (about to be recorded via PutBuildFingerprint) is now suspect.
// Each lost package simply has no UnitPointer, exactly the shape
// index.RevalidateStale already treats as one stale package needing a
// targeted repair (see internal/server's revalidateIndex) — scoped to just
// these packages, instead of forcing a full close-and-rebuild of a database
// that is otherwise current, the mechanism traced back from a cold-start
// build that reported complete progress yet was judged wholeDBStale
// immediately afterward.
func (r *buildResults) flushPendingLocked() {
	if len(r.pending) == 0 {
		return
	}
	batch := r.pending
	r.pending = nil
	if err := r.db.PutUnitsBatch(batch); err != nil {
		r.stats.Errors += len(batch)
		log.Printf("index: failed to commit a batch of %d unit(s): %v", len(batch), err)
	}
}

// flushPtrsLocked commits any pending pointer-only refresh batch. Unlike
// flushPendingLocked, a failure here is best-effort and not fatal: the
// affected packages' blob keys are already correct and unchanged (that is
// exactly why they were only queued for a pointer refresh, not a full
// UnitEntry write), so losing the refresh only costs a content-hash
// recheck on a future run instead of a stat-only skip — never correctness.
func (r *buildResults) flushPtrsLocked() {
	if len(r.pendingPtrs) == 0 {
		return
	}
	batch := r.pendingPtrs
	r.pendingPtrs = nil
	_ = r.db.PutUnitPointersBatch(batch)
}

func (r *buildResults) addErrLocked(err error) {
	if err == nil {
		return
	}
	if r.err == nil {
		r.err = err
		return
	}
	r.err = errors.Join(r.err, err)
}

// result returns the final Stats and the run's error, if any. A non-nil
// error here means Build's output is not trustworthy as a whole — a
// canceled context (see recordFatal), the only remaining source of this
// error since flushPendingLocked stopped treating a batch commit failure
// as fatal — not that any individual package failed to type-check or
// persist; those are only reflected in Stats.Errors. Call only after every
// worker has finished.
func (r *buildResults) result() (Stats, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats, r.err
}
