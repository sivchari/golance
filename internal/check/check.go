// Package check is an on-demand type checking engine: it type-checks the
// single Go package a file belongs to, resolving that package's
// dependencies from export data rather than re-checking them (see
// internal/typecheck). Results are cached per package directory, kept fresh
// against overlay/disk content via a content hash, and recomputed on a
// debounced, cancelable schedule as the editor reports changes.
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
	// recheck with a publishable summary of the result.
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

// dirState tracks the debounce timer and in-flight cancellation for one
// directory's recheck jobs. gen guards against a superseded job's
// completion clobbering a newer job's state.
type dirState struct {
	timer  *time.Timer
	cancel context.CancelFunc
	gen    uint64
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
// stale. It bypasses the debounce delay but still participates in per-dir
// job cancellation: a concurrent Invalidate for the same directory can
// cancel it.
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

	jctx, finish := e.startJob(ctx, dir)
	defer finish()
	return e.runRecheck(jctx, dir)
}

// Invalidate schedules a recheck of dir after Options.DebounceDelay of
// quiet. Repeated calls before the delay elapses reset the timer, so a
// burst of edits collapses into a single recheck. If a recheck for dir is
// still running when the delay elapses, it is canceled before the new one
// starts.
func (e *Engine) Invalidate(dir string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.jobs[dir]
	if !ok {
		st = &dirState{}
		e.jobs[dir] = st
	}
	if st.timer != nil {
		st.timer.Stop()
	}
	st.timer = time.AfterFunc(e.opts.DebounceDelay, func() { e.fireRecheck(dir) })
}

// fireRecheck runs the debounce-triggered recheck job for dir.
func (e *Engine) fireRecheck(dir string) {
	ctx, finish := e.startJob(context.Background(), dir)
	defer finish()
	_, _ = e.runRecheck(ctx, dir)
}

// startJob cancels any job already running for dir, registers a new
// cancelable context derived from parent, and returns it along with a
// finish func the caller must invoke when the job completes. finish is a
// no-op if a newer job has since superseded this one.
func (e *Engine) startJob(parent context.Context, dir string) (context.Context, func()) {
	e.mu.Lock()
	st, ok := e.jobs[dir]
	if !ok {
		st = &dirState{}
		e.jobs[dir] = st
	}
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	if st.cancel != nil {
		st.cancel()
	}
	st.gen++
	myGen := st.gen
	ctx, cancel := context.WithCancel(parent)
	st.cancel = cancel
	e.mu.Unlock()

	finish := func() {
		e.mu.Lock()
		if s, ok := e.jobs[dir]; ok && s.gen == myGen {
			s.cancel = nil
		}
		e.mu.Unlock()
	}
	return ctx, finish
}

// store caches cp for its directory, evicting the least recently used
// non-focused entry if the cache is at capacity.
func (e *Engine) store(dir string, cp *CheckedPackage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, exists := e.cache[dir]; !exists && len(e.cache) >= e.opts.MaxLRU {
		e.evictLocked()
	}
	e.cache[dir] = &cacheEntry{pkg: cp, lastUsed: time.Now()}
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
