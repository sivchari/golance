// Package check is an on-demand type checking engine: it type-checks the
// single Go package a file belongs to, resolving that package's
// dependencies from export data rather than re-checking them (see
// internal/typecheck). Results are cached per package directory, kept fresh
// against overlay/disk content via a content hash. Background rechecks
// (Invalidate) run on a debounced schedule and supersede-cancel one
// another; request-driven checks (Get) always run to completion against
// the content they read, like a gopls snapshot, and are never canceled by
// that schedule — see Get's doc for the invariant this implies.
package check

import (
	"context"
	"fmt"
	"go/types"
	"sync"
	"time"

	"github.com/sivchari/golance/internal/overlay"
)

// defaultMaxLRU and defaultDebounceDelay are the Options defaults used when
// the corresponding field is left zero.
const (
	defaultMaxLRU        = 6
	defaultDebounceDelay = 200 * time.Millisecond
)

// SnapshotSource resolves the package a source file belongs to. It is
// satisfied by GraphSource, an adapter over *graph.Snapshot.
type SnapshotSource interface {
	// PackageForFile returns the import path, directory, and known non-test
	// Go files of the package containing path. ok is false if path is not
	// part of any known package.
	PackageForFile(path string) (pkgPath string, dir string, goFiles []string, ok bool)
}

// Importer returns a types.ImporterFrom for resolving a recheck's
// dependencies. Engine calls it once per recheck. Unlike the package being
// checked (parsed into a fresh *token.FileSet every recheck, since its
// content changes), the returned importer is expected to be a shared,
// long-lived value backed by its own persistent *token.FileSet and
// typecheck.Cache: gcexportdata.Read ties a decoded *types.Package's
// position data to whichever fset was active at decode time, so an
// implementation that wants decode work cached across rechecks must keep
// resolving into the same fset for as long as it keeps that cache. See
// typecheck.Cache's doc for the invalidation contract this implies.
type Importer func() types.ImporterFrom

// Options configures an Engine. The zero value is valid: MaxLRU and
// DebounceDelay fall back to their defaults, and a nil OnResult simply
// disables result notifications.
type Options struct {
	// MaxLRU is the number of non-focused packages kept cached at once.
	// Defaults to 6.
	MaxLRU int
	// DebounceDelay is how long Invalidate waits for a directory to go
	// quiet before rechecking it. Defaults to 200ms.
	DebounceDelay time.Duration
	// OnResult, if set, is called after every successful (non-canceled)
	// recheck with a publishable summary of the result. Calls for the same
	// directory are serialized and strictly ordered by the recheck's
	// generation (see commitPublish): a slower-but-older recheck's call is
	// dropped rather than delivered after a faster-but-newer one's. OnResult
	// runs under dir's publish lock (dirState.pubMu) and must not call back
	// into Engine (Get, Invalidate, SetFocus, ...), synchronously or
	// otherwise, as that could deadlock against it.
	OnResult func(*Result)
}

// pkgInfo is what Engine remembers about a directory once it has resolved
// the package it holds: its import path and the non-test Go files
// SnapshotSource reported for it (used to determine the package's name when
// re-listing the directory).
type pkgInfo struct {
	pkgPath string
	goFiles []string
}

// cacheEntry is one cached CheckedPackage plus its recency, for LRU
// eviction.
type cacheEntry struct {
	pkg      *CheckedPackage
	lastUsed time.Time
}

// dirState tracks one directory's recheck bookkeeping.
//
// timer and cancel govern only the debounce-triggered background job
// (Invalidate/fireRecheck): timer is the pending debounce, cancel cancels
// the background job currently running, if any. epoch is bumped by
// startJob each time a new background job begins, so a stale job's finish
// func (see startJob) can tell it is no longer the current background job
// and must not clear a newer one's cancel func. None of this is touched by
// Get: request-driven checks neither register a cancel nor cancel one.
//
// gen, doneGen, pubMu, and pubGen implement the two completion-ordering
// guards described on Engine.commitCache and Engine.commitPublish: gen is
// the last generation handed out by nextGen for this directory. Both
// request-driven and background rechecks share this counter, since they
// can now complete in either order. doneGen (guarded by Engine.mu) is the
// highest generation whose cache write has been committed. pubGen (guarded
// by pubMu, a separate lock never held together with Engine.mu) is the
// highest generation whose Options.OnResult call has been made. Two
// separate gates are needed because computing a Result (Diagnostics reads
// files) and calling OnResult happen outside Engine.mu and can take
// unbounded time, so cache and publish ordering cannot be guaranteed by a
// single lock/check.
type dirState struct {
	timer  *time.Timer
	cancel context.CancelFunc
	epoch  uint64

	gen     uint64
	doneGen uint64

	pubMu  sync.Mutex
	pubGen uint64
}

// Engine is an on-demand type checking engine over a workspace. Safe for
// concurrent use.
type Engine struct {
	snap        SnapshotSource
	reader      overlay.FileReader
	newImporter Importer
	opts        Options

	mu    sync.Mutex
	focus string // directory of the focused package, "" if none
	dirs  map[string]pkgInfo
	cache map[string]*cacheEntry
	jobs  map[string]*dirState
}

// New returns an Engine that resolves files to packages via snap, reads
// content via reader, and builds a dependency importer via imp for every
// recheck.
func New(snap SnapshotSource, reader overlay.FileReader, imp Importer, opts Options) *Engine {
	if opts.MaxLRU <= 0 {
		opts.MaxLRU = defaultMaxLRU
	}
	if opts.DebounceDelay <= 0 {
		opts.DebounceDelay = defaultDebounceDelay
	}
	return &Engine{
		snap:        snap,
		reader:      reader,
		newImporter: imp,
		opts:        opts,
		dirs:        make(map[string]pkgInfo),
		cache:       make(map[string]*cacheEntry),
		jobs:        make(map[string]*dirState),
	}
}

// SetFocus marks the package containing filePath as focused, exempting it
// from LRU eviction until the next SetFocus call. A filePath SnapshotSource
// does not recognize is a no-op.
func (e *Engine) SetFocus(filePath string) {
	pkgPath, dir, goFiles, ok := e.snap.PackageForFile(filePath)
	if !ok {
		return
	}
	e.mu.Lock()
	e.focus = dir
	e.dirs[dir] = pkgInfo{pkgPath: pkgPath, goFiles: goFiles}
	e.mu.Unlock()
}

// Get returns the current CheckedPackage for the package containing
// filePath, type-checking it synchronously if the cache is missing or
// stale. It bypasses the debounce delay and, unlike a debounce-triggered
// background recheck, runs to completion against the content it read at
// the start of the recheck: it does not register for per-dir supersede
// cancellation, so a concurrent Invalidate/fireRecheck for the same
// directory can neither cancel it nor be canceled by it — the two may run
// concurrently. The only thing that cancels Get is ctx itself (the
// request's own context, canceled by $/cancelRequest or a dropped
// connection). Because a request-driven and a background recheck for the
// same directory can now finish in either order, Get's result is still
// gated by the generation guard in commit before it is cached or
// published; Get itself always returns the CheckedPackage it computed,
// regardless of that guard.
func (e *Engine) Get(ctx context.Context, filePath string) (*CheckedPackage, error) {
	pkgPath, dir, goFiles, ok := e.snap.PackageForFile(filePath)
	if !ok {
		return nil, fmt.Errorf("check: %s is not part of a known package", filePath)
	}
	pi := pkgInfo{pkgPath: pkgPath, goFiles: goFiles}

	e.mu.Lock()
	e.dirs[dir] = pi
	e.mu.Unlock()

	files, err := e.resolveFiles(pi, dir)
	if err != nil {
		return nil, err
	}
	hash, err := contentHash(e.reader, files)
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	if entry, ok := e.cache[dir]; ok && entry.pkg.contentHash == hash {
		entry.lastUsed = time.Now()
		cp := entry.pkg
		e.mu.Unlock()
		return cp, nil
	}
	e.mu.Unlock()

	return e.runRecheck(ctx, dir)
}

// Invalidate schedules a recheck of dir after Options.DebounceDelay of
// quiet. Repeated calls before the delay elapses reset the timer, so a
// burst of edits collapses into a single recheck. If a background recheck
// for dir is still running when the delay elapses, it is canceled before
// the new one starts. This never cancels a concurrent request-driven Get
// for the same directory; see Get's doc.
func (e *Engine) Invalidate(dir string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.jobStateLocked(dir)
	if st.timer != nil {
		st.timer.Stop()
	}
	st.timer = time.AfterFunc(e.opts.DebounceDelay, func() { e.fireRecheck(dir) })
}

// fireRecheck runs the debounce-triggered background recheck job for dir.
func (e *Engine) fireRecheck(dir string) {
	ctx, finish := e.startJob(context.Background(), dir)
	defer finish()
	_, _ = e.runRecheck(ctx, dir)
}

// jobStateLocked returns dir's dirState, creating it if necessary. Callers
// must hold e.mu.
func (e *Engine) jobStateLocked(dir string) *dirState {
	st, ok := e.jobs[dir]
	if !ok {
		st = &dirState{}
		e.jobs[dir] = st
	}
	return st
}

// startJob cancels any background job already running for dir, registers a
// new cancelable context derived from parent, and returns it along with a
// finish func the caller must invoke when the job completes. finish is a
// no-op if a newer background job has since superseded this one. This is
// used only for debounce-triggered background rechecks (fireRecheck); Get
// does not call it.
func (e *Engine) startJob(parent context.Context, dir string) (context.Context, func()) {
	e.mu.Lock()
	st := e.jobStateLocked(dir)
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	if st.cancel != nil {
		st.cancel()
	}
	st.epoch++
	myEpoch := st.epoch
	ctx, cancel := context.WithCancel(parent)
	st.cancel = cancel
	e.mu.Unlock()

	finish := func() {
		e.mu.Lock()
		if s, ok := e.jobs[dir]; ok && s.epoch == myEpoch {
			s.cancel = nil
		}
		e.mu.Unlock()
	}
	return ctx, finish
}

// nextGen assigns and returns dir's next monotonic generation number,
// creating its dirState if necessary. runRecheck calls this once per
// recheck attempt (both request-driven, via Get, and debounce-triggered,
// via fireRecheck), right before it starts reading file content: since
// neither kind of recheck can cancel the other anymore, they can finish in
// either order, so commit's completion-ordering guard uses gen — not
// completion order — to tell which of two concurrently running rechecks
// for the same directory reflects newer content.
func (e *Engine) nextGen(dir string) uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.jobStateLocked(dir)
	st.gen++
	return st.gen
}

// commit caches cp for dir and, if configured, publishes it via
// Options.OnResult, honoring dir's completion-ordering guards on both
// steps. gen, assigned by nextGen when the recheck started reading
// content, stands in for completion order: a request-driven Get and a
// background recheck for the same directory no longer cancel each other
// and so can complete in either order, and a slower-but-older recheck's
// result must not clobber a faster-but-newer one's — neither in the cache
// (commitCache) nor, independently, in what gets published (commitPublish;
// gating only the cache write is not enough, since computing and
// publishing a Result run outside Engine.mu and can take unbounded time).
func (e *Engine) commit(dir string, gen uint64, cp *CheckedPackage) {
	st, ok := e.commitCache(dir, gen, cp)
	if !ok || e.opts.OnResult == nil {
		return
	}
	e.commitPublish(gen, st, cp)
}

// commitCache stores cp in dir's cache slot under e.mu, evicting the least
// recently used non-focused entry if the cache is at capacity, unless a
// recheck with a higher generation for dir has already committed. ok is
// false, and nothing is written, if gen is stale. st is dir's dirState, for
// a subsequent commitPublish call.
func (e *Engine) commitCache(dir string, gen uint64, cp *CheckedPackage) (st *dirState, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st = e.jobStateLocked(dir)
	if gen < st.doneGen {
		return st, false
	}
	st.doneGen = gen
	if _, exists := e.cache[dir]; !exists && len(e.cache) >= e.opts.MaxLRU {
		e.evictLocked()
	}
	e.cache[dir] = &cacheEntry{pkg: cp, lastUsed: time.Now()}
	return st, true
}

// commitPublish computes cp's publishable Result and calls Options.OnResult
// with it, serialized and generation-ordered via st.pubMu — a lock
// dedicated to this purpose and never held together with e.mu, since
// Diagnostics reads files and OnResult publishes to the LSP client, neither
// of which may run while holding e.mu. A call whose gen is lower than the
// highest generation already published for dir is dropped without calling
// OnResult: because commitCache's gate alone only orders the cache write,
// an older-but-slower recheck that already passed it can still be
// mid-flight here (e.g. still computing Diagnostics) when a newer-but-
// faster recheck has already published; without this second gate it would
// publish after it, leaving the editor showing stale diagnostics until the
// next edit.
func (e *Engine) commitPublish(gen uint64, st *dirState, cp *CheckedPackage) {
	result := newResult(cp, Diagnostics(cp, e.reader))

	st.pubMu.Lock()
	defer st.pubMu.Unlock()
	if gen < st.pubGen {
		return
	}
	st.pubGen = gen
	e.opts.OnResult(result)
}

// Stop cancels every directory's pending debounce timer and in-flight
// background recheck (Invalidate/fireRecheck), so none of them can call
// Options.OnResult after the caller discards this Engine — e.g. because a
// fresh Engine over a new import graph snapshot is about to replace it.
// This has no effect on a request-driven Get already in flight: Get never
// registers with this per-dir bookkeeping (see its own doc), so it always
// runs to completion and is still gated at commit time by its own
// generation. Safe to call more than once.
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, st := range e.jobs {
		if st.timer != nil {
			st.timer.Stop()
			st.timer = nil
		}
		if st.cancel != nil {
			st.cancel()
		}
	}
}

// evictLocked removes the least recently used non-focused cache entry, if
// any. Callers must hold e.mu.
func (e *Engine) evictLocked() {
	var oldestDir string
	var oldestTime time.Time
	found := false
	for dir, entry := range e.cache {
		if dir == e.focus {
			continue
		}
		if !found || entry.lastUsed.Before(oldestTime) {
			oldestDir, oldestTime = dir, entry.lastUsed
			found = true
		}
	}
	if found {
		delete(e.cache, oldestDir)
	}
}
