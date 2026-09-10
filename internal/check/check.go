// Package check is an on-demand type checking engine: it type-checks the
// single Go package a file belongs to, resolving that package's
// dependencies from export data rather than re-checking them (see
// internal/typecheck). Results are cached per package directory, kept fresh
// against overlay/disk content via a content hash. Background rechecks
// (Invalidate) run on a debounced schedule and supersede-cancel one
// another; request-driven checks (Get) are deduplicated and run detached
// from any one requester, on the engine's own lifecycle ctx, so they always
// run to completion and warm the cache regardless of which caller (if any)
// is still waiting on them — see Get's doc for the invariant this implies.
package check

import (
	"context"
	"fmt"
	"go/types"
	"sync"
	"sync/atomic"
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

// pkgInfo is what Engine remembers about a unit once it has resolved the
// package it holds: its import path and the non-test Go files
// SnapshotSource reported for it (used to determine the package's name when
// re-listing the directory). For the external test variant, pkgPath carries
// externalTestPkgPathMarker and goFiles is empty (see
// GraphSource.PackageForFile and Engine.canonicalPackageName).
type pkgInfo struct {
	pkgPath string
	goFiles []string
}

// variant distinguishes which of a directory's (at most two) type-checked
// units a unitKey names. Before external test package support, a directory
// held at most one unit and dir alone was Engine's cache/dirs/jobs key; the
// directory's external "_test" package (see GraphSource.PackageForFile) is
// a second, independent unit over the same directory — different files,
// different pkgPath, its own recheck/cache/generation bookkeeping — so the
// key must carry which one a given entry is.
type variant int

// Variants a unitKey can name.
const (
	variantBase variant = iota
	variantExternalTest
)

// unitKey identifies one of Engine's type-checked units: a directory plus
// which variant of its package. See variant's doc.
type unitKey struct {
	dir     string
	variant variant
}

// unitKeyFor returns the unitKey a PackageForFile result identifies: the
// external test variant if pkgPath carries externalTestPkgPathMarker (see
// GraphSource.PackageForFile and externalTestVariant), the base variant
// otherwise.
func unitKeyFor(pkgPath, dir string) unitKey {
	if _, ok := externalTestVariant(pkgPath); ok {
		return unitKey{dir: dir, variant: variantExternalTest}
	}
	return unitKey{dir: dir, variant: variantBase}
}

// cacheEntry is one cached CheckedPackage plus its recency, for LRU
// eviction.
type cacheEntry struct {
	pkg      *CheckedPackage
	lastUsed time.Time
}

// dirState tracks one unit's (a directory plus variant, see unitKey) recheck
// bookkeeping.
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
// the last generation handed out by nextGen for this unit. Both
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

// flight is one in-flight, detached recheck for a unitKey+contentHash pair,
// shared by every concurrent Get that would otherwise redundantly recheck
// the exact same content. See Engine.Get's doc for the design this exists
// for (deduplication) and Engine.runFlight for the detachment half of it.
// cp and err are only valid after done is closed.
type flight struct {
	hash string
	done chan struct{}
	cp   *CheckedPackage
	err  error
}

// Engine is an on-demand type checking engine over a workspace. Safe for
// concurrent use.
type Engine struct {
	snap        SnapshotSource
	reader      overlay.FileReader
	newImporter Importer
	opts        Options

	// ctx and cancel are the engine's own lifecycle context: every flight
	// (see runFlight) runs on ctx, not on any individual Get caller's
	// request ctx, so it keeps running to completion — and can still warm
	// the cache — after a caller stops waiting on it. cancel is called by
	// Stop, which is the only thing that ends a flight early.
	ctx    context.Context
	cancel context.CancelFunc

	// retired is set by Retire. commit consults it to suppress
	// Options.OnResult for a recheck that completes after Retire — see
	// Retire's doc for why this, and not e.ctx cancellation, is how it stops
	// a retired Engine from publishing.
	retired atomic.Bool

	mu      sync.Mutex
	focus   string // directory of the focused package, "" if none — protects every variant of that directory from eviction, see evictLocked
	dirs    map[unitKey]pkgInfo
	cache   map[unitKey]*cacheEntry
	jobs    map[unitKey]*dirState
	flights map[unitKey]*flight

	// debounceWG counts debounce-triggered background rechecks that have
	// been armed but not yet resolved — see armDebounceLocked (Add, under
	// e.mu) and fireRecheck (Done, via defer). Wait blocks on it; Stop does
	// not, by design (see Stop's own doc), so this exists purely to give a
	// caller like a test's cleanup — which is about to delete the temp
	// directories a still-running recheck could still be reading from or
	// writing to — a way to block until that is no longer possible.
	debounceWG sync.WaitGroup
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
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{
		snap:        snap,
		reader:      reader,
		newImporter: imp,
		opts:        opts,
		ctx:         ctx,
		cancel:      cancel,
		dirs:        make(map[unitKey]pkgInfo),
		cache:       make(map[unitKey]*cacheEntry),
		jobs:        make(map[unitKey]*dirState),
		flights:     make(map[unitKey]*flight),
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
	e.dirs[unitKeyFor(pkgPath, dir)] = pkgInfo{pkgPath: pkgPath, goFiles: goFiles}
	e.mu.Unlock()
}

// Get returns the current CheckedPackage for the package containing
// filePath, type-checking it if the cache is missing or stale. It bypasses
// the debounce delay and, unlike a debounce-triggered background recheck,
// does not register for per-dir supersede cancellation, so a concurrent
// Invalidate/fireRecheck for the same directory can neither cancel it nor
// be canceled by it — the two may run concurrently.
//
// The recheck itself is deduplicated and detached: concurrent Gets for the
// same (unitKey, contentHash) join one shared flight (see runFlight)
// instead of each racing a redundant check, and that flight runs on the
// engine's own lifecycle ctx — never on any one caller's request ctx — so
// it always runs to completion and commits to the cache no matter which
// callers, if any, are still waiting on it when they stop waiting. This is
// what makes Get immune to an editor that cancels every hover on the next
// cursor move: that no longer prevents a slow first check from ever
// finishing, because the check keeps running in the background and the
// next request hits a warm cache instead of restarting from scratch. The
// only thing that can make Get itself return early is ctx — the request's
// own context, canceled by $/cancelRequest or a dropped connection: a
// waiter whose ctx is canceled stops waiting and returns ctx.Err()
// immediately, without affecting the flight it was joined to.
//
// Because a request-driven flight and a background recheck for the same
// directory can finish in either order, and because two flights racing an
// in-place edit (a new contentHash starts a fresh flight rather than
// joining a stale one — see getOrStartFlight) can also finish in either
// order, a flight's result is still gated by the generation guard in
// commit before it is cached or published; a waiter that does not hit
// ctx.Done() first always gets back the CheckedPackage its flight
// computed, regardless of that guard.
func (e *Engine) Get(ctx context.Context, filePath string) (*CheckedPackage, error) {
	// Checked up front so an already-canceled request fails before any
	// hashing work, and deterministically: the select below races
	// ctx.Done() against fl.done, and with both ready it picks either.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pkgPath, dir, goFiles, ok := e.snap.PackageForFile(filePath)
	if !ok {
		return nil, fmt.Errorf("check: %s is not part of a known package", filePath)
	}
	key := unitKeyFor(pkgPath, dir)
	pi := pkgInfo{pkgPath: pkgPath, goFiles: goFiles}

	e.mu.Lock()
	e.dirs[key] = pi
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
	if entry, ok := e.cache[key]; ok && entry.pkg.contentHash == hash {
		entry.lastUsed = time.Now()
		cp := entry.pkg
		e.mu.Unlock()
		return cp, nil
	}
	e.mu.Unlock()

	fl := e.getOrStartFlight(key, hash)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-fl.done:
		return fl.cp, fl.err
	}
}

// getOrStartFlight returns the in-flight recheck for key+hash, joining it
// if one is already running, or starting a fresh one (see runFlight)
// otherwise. hash is checked, not just key, because Get computes it before
// this call: an edit that lands mid-flight changes hash, and must start its
// own fresh flight rather than join one already committed to stale
// content — the flight it finds here, if any, could be running against
// content an edit has since superseded. A flight already running for a
// different (necessarily older) hash is left alone to run to completion
// undisturbed; callers simply do not join it, and its eventual commit loses
// to the newer flight's via commit's generation guard (see Get's doc).
func (e *Engine) getOrStartFlight(key unitKey, hash string) *flight {
	e.mu.Lock()
	if fl, ok := e.flights[key]; ok && fl.hash == hash {
		e.mu.Unlock()
		return fl
	}
	fl := &flight{hash: hash, done: make(chan struct{})}
	e.flights[key] = fl
	e.mu.Unlock()

	go e.runFlight(key, fl)
	return fl
}

// runFlight runs key's recheck to completion on e.ctx — the engine's own
// lifecycle context, canceled only by Stop, never by any individual Get
// caller's request ctx — and broadcasts the result to every Get waiting on
// fl.done, then removes fl from e.flights unless a newer flight has since
// replaced it there (see getOrStartFlight). Recovers a panic from the
// recheck so a bug there fails every current waiter with an error instead
// of crashing the whole process: unlike a synchronous per-request call,
// this goroutine is not covered by rpc.Server.callRequestHandler's own
// panic recovery, since it can outlive the request that started it.
func (e *Engine) runFlight(key unitKey, fl *flight) {
	defer func() {
		if r := recover(); r != nil {
			fl.cp, fl.err = nil, fmt.Errorf("check: panic during recheck of %s: %v", key.dir, r)
		}
		close(fl.done)
		e.mu.Lock()
		if cur, ok := e.flights[key]; ok && cur == fl {
			delete(e.flights, key)
		}
		e.mu.Unlock()
	}()
	fl.cp, fl.err = e.runRecheck(e.ctx, key)
}

// Invalidate schedules a recheck of dir's unit(s) after
// Options.DebounceDelay of quiet. Repeated calls before the delay elapses
// reset the timer, so a burst of edits collapses into a single recheck per
// unit. If a background recheck for a unit is still running when the delay
// elapses, it is canceled before the new one starts. This never cancels a
// concurrent request-driven Get for the same unit; see Get's doc.
//
// dir's base unit is always armed. Its external test unit (see
// GraphSource.PackageForFile) is armed too, but only if Engine has resolved
// one for dir before (via Get or SetFocus) — a directory's external test
// unit is the exception, not the rule, so arming a second debounce, probe,
// and doomed recheck attempt for every directory in the workspace on every
// edit, on the off chance it might have one, would cost every directory
// that never will to save one that might. A brand-new external test file's
// own first edit still resolves promptly: it goes through didOpen/Get
// (a request-driven check), which populates e.dirs for it before any
// Invalidate call needs to know to arm it.
func (e *Engine) Invalidate(dir string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	keys := []unitKey{{dir: dir, variant: variantBase}}
	if extKey := (unitKey{dir: dir, variant: variantExternalTest}); e.unitKnownLocked(extKey) {
		keys = append(keys, extKey)
	}
	for _, key := range keys {
		e.armDebounceLocked(key)
	}
}

// InvalidateDependency drops the cached CheckedPackage for each of dirs (both
// variants, see unitKey) that Engine already knows about (see
// unitKnownLocked), and arms the same debounce-triggered recheck+publish
// Invalidate does for those. A dir Engine has never resolved — never Get's
// or SetFocus'd, i.e. never opened — is left alone entirely: it has nothing
// cached that a stale dependency could have left stale in the first place
// (a content-hash cache hit can only ever return a previously committed
// entry), so there is nothing to drop and nothing worth scheduling a
// recheck for.
//
// This exists for Server.reindex's reverse-dependency closure of a saved
// package: unlike Invalidate's own callers (didOpen/didChange/didSave, each
// scoped to the one file its own notification is about, always already
// known by the time it calls Invalidate — see Invalidate's doc), dirs here
// can span an arbitrary number of OTHER packages nothing in that
// notification touched directly, most of which — in a large workspace — the
// engine may never have resolved at all. Arming Invalidate's own
// unconditional debounce for every one of them would schedule a full
// recheck, and publish diagnostics, for a file the user never opened, on
// every save of a widely-imported package; gating on unitKnownLocked instead
// bounds the work (and the diagnostics traffic) to units the engine already
// has live, which is exactly the set whose cached type information this
// dependency change could have made wrong.
func (e *Engine) InvalidateDependency(dirs []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, dir := range dirs {
		for _, v := range [...]variant{variantBase, variantExternalTest} {
			key := unitKey{dir: dir, variant: v}
			if !e.unitKnownLocked(key) {
				continue
			}
			delete(e.cache, key)
			e.armDebounceLocked(key)
		}
	}
}

// unitKnownLocked reports whether Engine has ever resolved or cached key.
// Callers must hold e.mu.
func (e *Engine) unitKnownLocked(key unitKey) bool {
	if _, ok := e.dirs[key]; ok {
		return true
	}
	_, ok := e.cache[key]
	return ok
}

// armDebounceLocked (re)starts key's debounce timer. Callers must hold e.mu.
func (e *Engine) armDebounceLocked(key unitKey) {
	st := e.jobStateLocked(key)
	if st.timer != nil {
		if st.timer.Stop() {
			// The timer being superseded never fired, so fireRecheck's
			// defer e.debounceWG.Done() (matching the Add below) will
			// never run for it; balance it here instead. If Stop returns
			// false the timer had already fired (or been stopped once
			// already) and that invocation owns its own Done call.
			e.debounceWG.Done()
		}
	}
	e.debounceWG.Add(1)
	st.timer = time.AfterFunc(e.opts.DebounceDelay, func() { e.fireRecheck(key) })
}

// fireRecheck runs the debounce-triggered background recheck job for key,
// retrying once, immediately, if the attempt fails for a reason other than
// ctx being canceled. Unlike Get, nothing is waiting on this call to notice
// a failure and retry it itself — without this, a momentary read error (a
// directory listing or file read racing an external rewrite, e.g. a git
// checkout or the editor's own atomic save landing mid-read) would discard
// runRecheck's result outright and leave the client's diagnostics frozen at
// whatever was last published, with nothing left to ever retrigger a check
// for key again. A canceled ctx (Stop, or a newer debounce superseding this
// one via startJob) is not a failure and is never retried. A retry that
// fails again is left alone rather than retried further here — the same
// "at most once" self-heal bound this package's callers already rely on
// elsewhere (see internal/server's loadWorkspaceAsync) — so a persistent
// failure is picked up by the next independent trigger (a further edit, a
// watched-file event) instead of retried in a loop.
func (e *Engine) fireRecheck(key unitKey) {
	defer e.debounceWG.Done()
	ctx, finish := e.startJob(context.Background(), key)
	defer finish()
	if _, err := e.runRecheck(ctx, key); err != nil && ctx.Err() == nil {
		_, _ = e.runRecheck(ctx, key)
	}
}

// jobStateLocked returns key's dirState, creating it if necessary. Callers
// must hold e.mu.
func (e *Engine) jobStateLocked(key unitKey) *dirState {
	st, ok := e.jobs[key]
	if !ok {
		st = &dirState{}
		e.jobs[key] = st
	}
	return st
}

// startJob cancels any background job already running for key, registers a
// new cancelable context derived from parent, and returns it along with a
// finish func the caller must invoke when the job completes. finish is a
// no-op if a newer background job has since superseded this one. This is
// used only for debounce-triggered background rechecks (fireRecheck); Get
// does not call it.
//
// If e.ctx is already canceled — Stop has already run, or is running
// concurrently and reaches its own e.mu section either before or after this
// one — the returned context is pre-canceled and never registered as key's
// dirState.cancel. This is what makes Stop's contract airtight against a
// debounce timer that fires concurrently with Stop itself (see
// armDebounceLocked's time.AfterFunc: Stop cannot prevent an already-fired
// timer's callback from running, only from doing anything once it does):
// whichever of the two goroutines reaches e.mu first, the other observes a
// fully consistent outcome — either this job registers before Stop's own
// pass, and Stop's loop below cancels it like any other, or Stop's
// e.cancel() has already run, and this call sees e.ctx.Err() != nil and
// bails before registering anything Stop could otherwise miss. Either way,
// fireRecheck's caller ends up with a context runRecheck rejects at its very
// first check (before any file I/O or type-checking), so it can never reach
// commit/Options.OnResult once Stop has returned.
func (e *Engine) startJob(parent context.Context, key unitKey) (context.Context, func()) {
	e.mu.Lock()
	if e.ctx.Err() != nil {
		e.mu.Unlock()
		ctx, cancel := context.WithCancel(parent)
		cancel()
		return ctx, func() {}
	}
	st := e.jobStateLocked(key)
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
		if s, ok := e.jobs[key]; ok && s.epoch == myEpoch {
			s.cancel = nil
		}
		e.mu.Unlock()
	}
	return ctx, finish
}

// nextGen assigns and returns key's next monotonic generation number,
// creating its dirState if necessary. runRecheck calls this once per
// recheck attempt (both request-driven, via Get, and debounce-triggered,
// via fireRecheck), right before it starts reading file content: since
// neither kind of recheck can cancel the other anymore, they can finish in
// either order, so commit's completion-ordering guard uses gen — not
// completion order — to tell which of two concurrently running rechecks
// for the same unit reflects newer content.
func (e *Engine) nextGen(key unitKey) uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.jobStateLocked(key)
	st.gen++
	return st.gen
}

// commit caches cp for key and, if configured, publishes it via
// Options.OnResult, honoring key's completion-ordering guards on both
// steps. gen, assigned by nextGen when the recheck started reading
// content, stands in for completion order: a request-driven Get and a
// background recheck for the same unit no longer cancel each other
// and so can complete in either order, and a slower-but-older recheck's
// result must not clobber a faster-but-newer one's — neither in the cache
// (commitCache) nor, independently, in what gets published (commitPublish;
// gating only the cache write is not enough, since computing and
// publishing a Result run outside Engine.mu and can take unbounded time).
//
// The cache write always happens, even after Retire: it is harmless on an
// engine about to be discarded, and a Get waiter joined to this recheck's
// flight reads fl.cp directly rather than going back through the cache (see
// Get's doc), so skipping it would save nothing. Only the OnResult publish
// is suppressed once e.retired is set — see Retire's doc.
func (e *Engine) commit(key unitKey, gen uint64, cp *CheckedPackage) {
	st, ok := e.commitCache(key, gen, cp)
	if !ok || e.opts.OnResult == nil || e.retired.Load() {
		return
	}
	e.commitPublish(gen, st, cp)
}

// commitCache stores cp in key's cache slot under e.mu, evicting the least
// recently used non-focused entry if the cache is at capacity, unless a
// recheck with a higher generation for key has already committed. ok is
// false, and nothing is written, if gen is stale. st is key's dirState, for
// a subsequent commitPublish call.
func (e *Engine) commitCache(key unitKey, gen uint64, cp *CheckedPackage) (st *dirState, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st = e.jobStateLocked(key)
	if gen < st.doneGen {
		return st, false
	}
	st.doneGen = gen
	if _, exists := e.cache[key]; !exists && len(e.cache) >= e.opts.MaxLRU {
		e.evictLocked()
	}
	e.cache[key] = &cacheEntry{pkg: cp, lastUsed: time.Now()}
	return st, true
}

// commitPublish computes cp's publishable Result and calls Options.OnResult
// with it, serialized and generation-ordered via st.pubMu — a lock
// dedicated to this purpose and never held together with e.mu, since
// Diagnostics reads files and OnResult publishes to the LSP client, neither
// of which may run while holding e.mu. A call whose gen is lower than the
// highest generation already published for key is dropped without calling
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

// Retire cancels every directory's pending debounce timer and in-flight
// background recheck (Invalidate/fireRecheck), same as Stop, but — unlike
// Stop — does not cancel e.ctx, so a request-driven flight already in
// progress (see runFlight, which runs on e.ctx) keeps running to completion
// instead of failing every Get waiting on it with ctx.Err().
//
// This is for a caller that discards e for a fresh Engine (e.g. over a new
// import graph snapshot) but must stop e from publishing afterward without
// aborting request work already in flight against it: canceling every
// pending timer and background job stops any of them from reaching commit
// after Retire returns, and setting e.retired makes commit itself suppress
// the OnResult publish for the rare recheck already past that point when
// Retire is called (see commit's doc) — while every in-flight flight is left
// to run to completion, so its waiters still get a real result instead of a
// spurious cancellation.
//
// Safe to call more than once.
func (e *Engine) Retire() {
	e.retired.Store(true)
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

// Stop fully shuts e down: it cancels every directory's pending debounce
// timer and in-flight background recheck (Invalidate/fireRecheck), and
// cancels e.ctx — which every request-driven flight runs on (see
// runFlight) — canceling every flight currently in progress too, so none
// of them can call Options.OnResult once e is discarded. A flight's
// detachment (see Get's doc) is only from any single requester's ctx, not
// from the engine's own lifetime: Stop still reclaims it. See Retire for
// the alternative that stops background publishing without aborting
// in-flight request-driven work.
//
// This contract holds even for a debounce timer that fires concurrently
// with Stop itself: time.Timer.Stop cannot prevent an already-fired
// timer's callback from running, so this loop alone cannot guarantee such a
// callback never calls Options.OnResult — the guarantee instead comes from
// startJob observing e.ctx (canceled above, under the same e.mu this loop
// holds) before registering any job Stop could otherwise race past. See
// startJob's doc for the full argument. No debounce-triggered background
// recheck can reach commit/Options.OnResult once Stop has returned,
// regardless of how its timer's fire raced this call.
// Safe to call more than once.
//
// Stop deliberately does not wait for an already-running recheck's I/O to
// actually finish (cancellation only takes effect at runRecheck's next
// ctx.Err() check, not instantaneously) — a caller that needs that
// guarantee, e.g. before deleting the temp directories a recheck's disk
// reads or dependency-export writes could still be touching, should call
// Wait after Stop.
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cancel()
	for _, st := range e.jobs {
		if st.timer != nil {
			if st.timer.Stop() {
				// Canceled before it ever fired, so fireRecheck's own
				// Done (matching armDebounceLocked's Add) will never run
				// for it — balance debounceWG here, mirroring
				// armDebounceLocked's identical re-arm case.
				e.debounceWG.Done()
			}
			st.timer = nil
		}
		if st.cancel != nil {
			st.cancel()
		}
	}
}

// Wait blocks until every debounce-triggered background recheck that had
// already been armed or started before this call returns — i.e. until
// none can still be reading or writing anything. It does not itself
// cancel or stop anything; call Stop first so no new debounce timer can
// arm after Wait begins (Wait does not block a concurrent Invalidate from
// arming a fresh one, which would otherwise never be observed).
func (e *Engine) Wait() {
	e.debounceWG.Wait()
}

// evictLocked removes the least recently used cache entry outside the
// focused directory (in either variant — see unitKey — SetFocus only
// records a directory, so a focused directory's external test unit is
// protected right alongside its base unit), if any. Callers must hold e.mu.
func (e *Engine) evictLocked() {
	var oldestKey unitKey
	var oldestTime time.Time
	found := false
	for key, entry := range e.cache {
		if key.dir == e.focus {
			continue
		}
		if !found || entry.lastUsed.Before(oldestTime) {
			oldestKey, oldestTime = key, entry.lastUsed
			found = true
		}
	}
	if found {
		delete(e.cache, oldestKey)
	}
}
