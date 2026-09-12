package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"go.lsp.dev/protocol"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/xref"
)

// Environment variables golance uses to configure and detect the indexer
// subprocess. Server sets these when it launches cmd/golance again as the
// indexer; cmd/golance reads EnvIndexer to decide which mode to run in.
const (
	// EnvIndexer, set to "1", tells cmd/golance to run as a one-shot
	// indexer (internal/index.Build) instead of starting the LSP server.
	EnvIndexer = "GOLANCE_INDEXER"
	// EnvRoot is the workspace root the indexer subprocess builds a facts
	// index for.
	EnvRoot = "GOLANCE_ROOT"
	// EnvDB is the per-root index database file path the indexer
	// subprocess writes to (see indexDBFile).
	EnvDB = "GOLANCE_DB"
	// EnvCAS is the content-addressed blob store directory the indexer
	// subprocess writes to (see casDir) — shared across every worktree of
	// the same repository.
	EnvCAS = "GOLANCE_CAS"
	// EnvDepCAS is the machine-global, content-addressed store directory
	// the indexer subprocess persists non-root dependency export data into
	// (see depExportCASDir and internal/depexport's package doc) — shared
	// across every repository this machine ever indexes, unlike EnvCAS.
	EnvDepCAS = "GOLANCE_DEP_CAS"
	// EnvIndexJobs mirrors the -index-jobs flag: index.Options.Parallelism.
	// Empty or non-numeric uses index's own default.
	EnvIndexJobs = "GOLANCE_INDEX_JOBS"
	// EnvOffline mirrors the -offline flag: "1" forbids module downloads
	// (GOPROXY=off) during graph load and indexing.
	EnvOffline = "GOLANCE_OFFLINE"
)

// repoKey returns the identity golance uses to key the shared CAS directory
// (see casDir) and to decide the facts index's path storage format (see
// RelativeIndexPaths): the absolute path of `git rev-parse --git-common-dir`
// run in root, when root is inside a git repository — every worktree of the
// same repository shares this one key, since --git-common-dir always
// resolves to the same directory regardless of which worktree it is run
// from — or root itself, with shared=false, otherwise (a plain non-git
// workspace, which gets its own private CAS exactly as golance behaved
// before worktree sharing existed; there is nothing to share, and no
// benefit to paying the relative-path bookkeeping for it).
//
// A thin wrapper over graph.RepoKey, which internal/graph's own cache
// (graph.CacheFile/Shared) uses for the identical decision over the import
// graph cache: keeping exactly one implementation of the underlying git
// invocation means the CAS, facts index, and graph cache can never
// disagree about which worktrees share what.
func repoKey(root string) (key string, shared bool) {
	return graph.RepoKey(root)
}

// RelativeIndexPaths reports whether root's facts index (CAS blobs and the
// per-root index database) stores source file paths relative to root (see
// internal/index.Options.RelativePaths) rather than as absolute paths. It
// is root's git-repository test — the same one casDir uses to key the
// shared CAS — exported so cmd/golance's indexer subprocess and every
// reader (internal/xref.New, index.Revalidate) can independently recompute
// the same answer a given database and CAS were written with, without
// needing it threaded through an extra environment variable or stored
// flag: a CAS blob's path format is a fixed, deterministic function of
// root's git-repository-ness, so it can never disagree.
func RelativeIndexPaths(root string) bool {
	_, shared := repoKey(root)
	return shared
}

// casDir returns the content-addressed blob store directory shared by
// every worktree of root's repository (see repoKey), under
// $XDG_CACHE_HOME (or the platform default via os.UserCacheDir). Unlike
// indexDBFile, this is never root-private: a blob's own key already
// captures everything about its content (see the internal/store package
// doc), so there is nothing two worktrees writing the same content could
// corrupt in each other by sharing this directory, and no lock is ever
// needed to do so.
func casDir(root string) string {
	key, _ := repoKey(root)
	h := sha256.Sum256([]byte(key))
	return filepath.Join(cacheBaseDir(), "golance", fmt.Sprintf("cas-%x", h[:8]))
}

// depExportCASDir returns the machine-global content-addressed store
// directory for internal/depexport's dependency export-data cache (see its
// own package doc): unlike casDir, this is never per-repository — a
// standard-library or module-cache dependency's checked export data is
// identical no matter which repository asked for it first, so every
// worktree of every repository this machine ever opens shares one
// directory, computed once per dependency ever instead of once per
// repository.
func depExportCASDir() string {
	return filepath.Join(cacheBaseDir(), "golance", "depexport")
}

// indexDBFile returns the per-root index database path for root: the small
// bbolt database mapping each package to its current CAS blob key plus the
// name/method/SymbolID-string lookup indices (see the internal/store
// package doc). Unlike the pre-redesign single shared database, this is
// always private to root — never shared with another worktree, even of the
// same repository — so two golance sessions for different worktrees never
// contend for it. Two golance sessions for the *same* root (e.g. the same
// folder open in two editor windows) still contend on this one file's
// exclusive lock, exactly as the pre-redesign database did; see
// privateIndexDBFile and switchToPrivateIndex for how a second such session
// now falls back instead of losing cross-reference functionality entirely.
func indexDBFile(root string) string {
	return cacheDBFile("index", root)
}

// privateIndexInfix marks a per-session-private index database file (see
// privateIndexDBFile), embedded in its filename between indexDBFile's own
// name and the .db extension so cleanupOrphanedPrivateIndexes' glob can
// recognize one without ever matching the shared file itself, whose name
// never contains this substring.
const privateIndexInfix = ".private-"

// privateIndexDBFile returns the session-private index database path this
// session uses for root once it has switched away from the shared one (see
// switchToPrivateIndex): the same base name indexDBFile(root) would use,
// with sessionID spliced in via privateIndexInfix so two sessions (or, in a
// test binary, two Server instances sharing one process) never collide on
// the same private path. Never shared with any other session — maintained
// solely by this session's own indexer subprocess and didSave-triggered
// reindexes for as long as the session lives (see Server.Stop), giving it
// full cross-reference functionality independent of whichever session
// currently holds the shared database's lock.
//
// Future work (out of scope for this fallback): golance does not attempt
// to reconcile a session-private index back into the shared one, run any
// daemon to arbitrate a single shared writer, or adopt the shared file
// read-only mid-session once its lock frees up. A session that fell back
// to a private index keeps using it for its own remaining lifetime.
func privateIndexDBFile(root, sessionID string) string {
	shared := indexDBFile(root)
	return strings.TrimSuffix(shared, ".db") + privateIndexInfix + sessionID + ".db"
}

// privateIndexGlobPattern returns the filepath.Glob pattern matching every
// session-private index database file for root, live or orphaned — used by
// cleanupOrphanedPrivateIndexes at startup. It never matches the shared
// index file itself (see privateIndexInfix).
func privateIndexGlobPattern(root string) string {
	shared := indexDBFile(root)
	return strings.TrimSuffix(shared, ".db") + privateIndexInfix + "*.db"
}

// newSessionID returns an identifier unique to one Server instance, not
// just one OS process: two Server values constructed within the same test
// binary (sharing one PID) must still resolve to different private index
// paths (see privateIndexDBFile), so a PID alone is not enough. Random
// bytes make collision effectively impossible regardless of how many
// Server instances a given process ever creates.
func newSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Exceedingly unlikely (crypto/rand failing means the OS's own
		// entropy source is broken), but a session must still get some
		// usable, if weaker, identifier rather than fail to start.
		return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return fmt.Sprintf("%d-%x", os.Getpid(), b)
}

func cacheDBFile(prefix, key string) string {
	h := sha256.Sum256([]byte(key))
	return filepath.Join(cacheBaseDir(), "golance", fmt.Sprintf("%s-%x.db", prefix, h[:8]))
}

func cacheBaseDir() string {
	if base, err := os.UserCacheDir(); err == nil {
		return base
	}
	return filepath.Join(os.Getenv("HOME"), ".cache")
}

// dbPath returns the per-root index database path this session uses for
// root: the shared one (indexDBFile) in the ordinary case, or this
// session's own private one (privateIndexDBFile) once switchToPrivateIndex
// has recorded that the shared database is locked by another live session.
// Unlike the pre-fallback design, this is no longer a pure function of root
// alone — s.usePrivateIndex makes the switch sticky for the rest of the
// session, so every caller (tryWarmOpen, buildIndexLocked, revalidateIndex
// via buildIndexLocked, and Stop's own cleanup) keeps agreeing on the same
// path once it has been made.
func (s *Server) dbPath(root string) string {
	if s.usePrivateIndex.Load() {
		return privateIndexDBFile(root, s.sessionID)
	}
	return indexDBFile(root)
}

// switchToPrivateIndex records that this session has fallen back to its own
// private facts index (see privateIndexDBFile) after finding the shared one
// locked by another live session, and logs one clear informational message
// about it — deliberately via logMessage (the client's log/output panel),
// not showMessage's modal popup, since this is an expected, self-resolving
// situation for a normal multi-editor workflow, not a failure the user
// needs to act on (contrast warnIndexUnavailable). Idempotent and safe to
// call from multiple call sites that can each independently detect the same
// lock (tryWarmOpen, and buildIndexLocked's own retry) — only the first
// call within a session logs anything.
func (s *Server) switchToPrivateIndex() {
	if !s.usePrivateIndex.CompareAndSwap(false, true) {
		return
	}
	const msg = "shared index locked by another session; building a session-private index"
	s.logger.Printf("golance: %s", msg)
	s.logMessage("golance: " + msg)
}

// tryWarmOpen opens root's existing per-root index database and CAS
// directly, if the database exists, without waiting for any revalidation.
// This lets cross-reference queries answer immediately on a second-or-later
// session instead of waiting for a rebuild pass that — CAS-hit fast paths
// aside — still has to enumerate every workspace package before confirming
// nothing changed. It reports ok=false if no database exists yet, in which
// case the caller should fall back to buildIndex.
//
// If the shared database exists but is currently locked by another live
// session (store.IsLocked), this switches the session over to its own
// private index (switchToPrivateIndex) and reports ok=false exactly as the
// "no database yet" case does: buildIndex's subsequent dbPath call then
// resolves to the private path, so it builds and opens that instead of
// repeating the same failed shared-path attempt.
//
// The database opened here may be stale — built under a different
// toolchain, or missing changes made outside this session since it was
// last built — since this does not check the build fingerprint or
// otherwise revalidate anything. handleInitialize pairs a successful
// warm-open with revalidateIndex, which checks in the background and
// triggers a rebuild if it finds anything stale; until that finishes,
// queries simply answer from whatever this opened.
// newResolver builds an xref.Resolver over db/cas/snap, wired with s's own
// logger (xref.Resolver.SetLogger) so an implementation query that comes
// back empty leaves a server-side diagnostic trail explaining why (see
// implementation.go's implDiag) instead of silence indistinguishable from
// "genuinely no implementers" -- the same silent gap a real monorepo
// report traced to an interface method signature referencing a module
// dependency's type. Every indexState construction site (tryWarmOpen,
// buildIndexLocked, workspace.go's revalidateGraph) goes through this
// instead of calling xref.New directly, so none of them can forget it.
func (s *Server) newResolver(db *store.DB, cas *store.CAS, snap *graph.Snapshot, relative bool) *xref.Resolver {
	r := xref.New(db, cas, snap, relative)
	r.SetLogger(s.logger)
	return r
}

func (s *Server) tryWarmOpen(root string) (*indexState, bool) {
	dbPath := s.dbPath(root)
	if _, err := os.Stat(dbPath); err != nil {
		return nil, false
	}
	db, err := store.Open(dbPath)
	if err != nil {
		if store.IsLocked(err) {
			s.switchToPrivateIndex()
		} else {
			s.logger.Printf("golance: warm-open index: %v", err)
		}
		return nil, false
	}
	cas, err := store.OpenCAS(casDir(root))
	if err != nil {
		s.logger.Printf("golance: warm-open CAS: %v", err)
		_ = db.Close()
		return nil, false
	}
	ws := s.workspace()
	if ws == nil {
		_ = db.Close()
		return nil, false
	}
	return &indexState{db: db, cas: cas, resolver: s.newResolver(db, cas, ws.snap, RelativeIndexPaths(root))}, true
}

// indexRepairThreshold bounds how many stale root packages revalidateIndex
// repairs in place (repairIndexPackagesLocked) rather than falling back to
// a full close-and-rebuild (buildIndexLocked). A targeted repair walks each
// stale package's reverse-dependency closure sequentially, in-process,
// while a full rebuild parallelizes every package across the indexer
// subprocess's own worker pool (see index.Build's Options.Parallelism) —
// worthwhile once the stale set is large enough that a sequential walk
// would cost more than the parallelized alternative, or is simply close to
// the whole workspace anyway. A var, not a const, so a test can lower it to
// exercise the full-rebuild fallback without needing a fixture with 64+
// stale packages.
var indexRepairThreshold = 64

// indexRevalidateAction is revalidateIndex's decision for what to do with a
// staleIndexPackages result, split out via chooseIndexRevalidateAction so
// the decision itself can be unit-tested without triggering either a real
// reindex or an indexer subprocess launch — the same "test the decision,
// not the dispatch" split workspace.go's workspaceReadyRefreshes uses for
// an analogous reason.
type indexRevalidateAction int

const (
	indexRevalidateNone indexRevalidateAction = iota
	indexRevalidateRepair
	indexRevalidateRebuild
)

// chooseIndexRevalidateAction decides revalidateIndex's branch from a
// staleIndexPackages result: no-op when nothing is stale, a targeted
// in-place repair when the stale set is small enough (indexRepairThreshold)
// and the whole database is still trustworthy, or a full rebuild otherwise —
// including whenever wholeDBStale is true, since Reindex never writes a
// build fingerprint (see index.RevalidateStale's own doc) and so cannot
// resolve that case no matter how few packages came back stale.
func chooseIndexRevalidateAction(pkgs []string, wholeDBStale bool) indexRevalidateAction {
	if !wholeDBStale && len(pkgs) == 0 {
		return indexRevalidateNone
	}
	if !wholeDBStale && len(pkgs) <= indexRepairThreshold {
		return indexRevalidateRepair
	}
	return indexRevalidateRebuild
}

// revalidateIndex checks, cheaply and in-process, whether root's
// warm-opened facts index (installed by a prior tryWarmOpen) is still up
// to date — the same skip logic [index.Build] uses to decide whether a
// package needs rechecking, minus any type-checking itself (see
// index.RevalidateStale). This runs concurrently with query handling and
// any in-session Reindex against the same *store.DB (bbolt supports any
// number of concurrent readers alongside one writer on one open handle),
// so nothing needs to pause while it runs.
//
// If nothing is stale, this is a no-op: the warm-opened index keeps
// serving as-is. If the stale set is small (chooseIndexRevalidateAction),
// each stale package is repaired in place (repairIndexPackagesLocked) —
// the same in-process s.reindex call handleDidSave makes — without ever
// nil-ing out s.idx, so cross-reference queries keep answering throughout.
// Otherwise — a toolchain change, or a stale set large enough that a
// sequential repair no longer pays for itself — it closes the warm-opened
// db handle (releasing it; bbolt's Close blocks until any in-flight read
// finishes, so this does not race a concurrent query) and falls back to
// buildIndex, the same full-rebuild path a cold start (no warm-open at
// all) uses.
//
// v0.1 scope: this check runs once per caller (see below), not on an
// ongoing poll for external file changes during the rest of the session.
//
// revalidateIndex has several independent callers that can fire close
// enough together to both observe the same warm-opened index as stale at
// once: the once-per-session background check right after initialize
// (lifecycle.go), a watched-files-triggered revalidateWorkspace pass
// (workspace.go), and revalidateGraph itself, which every reload — of
// either kind — ultimately runs through. s.idxMu (held for this call's
// entire body, including any repair or rebuild it triggers) serializes all
// of them: the second caller through the lock re-checks
// staleIndexPackages against whatever the first one just installed, so it
// only acts again if still actually necessary, never races the first's own
// Store(nil)/Close, and never runs a second indexer subprocess
// concurrently with the first's.
func (s *Server) revalidateIndex(ctx context.Context, root string) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	pkgs, wholeDBStale := s.staleIndexPackages(ctx)
	action := chooseIndexRevalidateAction(pkgs, wholeDBStale)
	if action == indexRevalidateRepair {
		idx := s.idx.Load()
		ws := s.workspace()
		if idx != nil && ws != nil {
			s.repairIndexPackagesLocked(ctx, ws, idx, pkgs)
			return
		}
		// idx or ws vanished under us since staleIndexPackages read them
		// (e.g. a concurrent Stop): a full rebuild's own workspace()/idx
		// re-checks handle that safely, whereas silently doing nothing
		// would leave the stale packages unrepaired.
		action = indexRevalidateRebuild
	}
	if action == indexRevalidateNone {
		return
	}
	if idx := s.idx.Load(); idx != nil {
		s.idx.Store(nil)
		// Wait for every detached reindex already registered against idx
		// (documentsync.go's beginReindex/reindexIfStillCurrent) to finish
		// its own write before closing the database out from under it. Safe
		// against a new registration racing this wait: beginReindex also
		// requires s.idxMu, held here for revalidateIndex's entire body, so
		// nothing can register between the Store(nil) above and the Close
		// below — see reindexWG's own doc.
		s.reindexWG.Wait()
		if err := idx.db.Close(); err != nil {
			s.logger.Printf("golance: close index before rebuild: %v", err)
		}
	}
	s.buildIndexLocked(ctx, root)
}

// repairIndexPackagesLocked reindexes each of pkgs in place, via the same
// s.reindex call handleDidSave makes for a saved package — including its
// reverse-dependency-closure fan-out and depCache/resolver invalidation.
// Unlike buildIndexLocked's close-and-rebuild path, s.idx is never touched:
// cross-reference queries keep answering throughout, from increasingly
// fresh state as each package is repaired, rather than returning
// index-unavailable for however long a full subprocess rebuild would take.
// Called only from revalidateIndex, which already holds s.idxMu.
//
// A package s.reindex fails to repair is left exactly as stale as it was
// before this call (reindex writes nothing on failure), same as always, but
// unlike before this now tells the client about it via one window/
// logMessage listing every such package, instead of only s.reindex's own
// server-side log line: revalidateIndex's repair pass runs at most once per
// loadWorkspaceAsync call (see its own doc) and nothing else retries a
// package left unrepaired here until its own next independent trigger (an
// edit, a watched-file event), so without this the only trace of a
// deterministically-failing package was a log file most users never open —
// see loadWorkspaceAsync's doc for the self-heal contract this reports on.
// logMessage, not showMessage: like resolverOrWarn's own "index still
// building" notice, this describes an expected, self-resolving gap (the
// next edit to any of these packages repairs it) rather than a failure
// severe enough to warrant a modal.
func (s *Server) repairIndexPackagesLocked(ctx context.Context, ws *workspace, idx *indexState, pkgs []string) {
	var failed []string
	for _, pkgPath := range pkgs {
		if err := s.reindex(ctx, ws, idx, pkgPath); err != nil {
			failed = append(failed, pkgPath)
		}
	}
	if len(failed) > 0 {
		s.logMessage(fmt.Sprintf("golance: %d package(s) failed to self-heal during startup index revalidation and remain stale until next edited: %s", len(failed), strings.Join(failed, ", ")))
	}
}

// staleIndexPackages reports which of the currently warm-opened index's
// (if any) root packages are stale, per index.RevalidateStale — the
// per-package detail revalidateIndex needs to decide between a targeted
// repair and a full rebuild, rather than index.Revalidate's single
// whole-database bool. Reports nothing stale (nil, false) whenever there is
// nothing warm-opened to check, or the check itself fails (conservatively:
// keep serving what is already open rather than force a rebuild on every
// transient error).
func (s *Server) staleIndexPackages(ctx context.Context) (pkgs []string, wholeDBStale bool) {
	idx := s.idx.Load()
	if idx == nil {
		return nil, false
	}
	ws := s.workspace()
	if ws == nil {
		return nil, false
	}
	pkgs, wholeDBStale, err := index.RevalidateStale(ctx, ws.snap, idx.db, runtime.Version(), "", RelativeIndexPaths(ws.root))
	if err != nil {
		s.logger.Printf("golance: revalidate index: %v", err)
		return nil, false
	}
	return pkgs, wholeDBStale
}

// buildIndex launches the indexer subprocess for root, relays its build
// progress as $/progress notifications, and opens the resulting index on
// success. An indexer failure is reported via window/showMessage;
// cross-reference features stay unavailable until the next successful
// build, but interactive features (hover, completion, diagnostics) are
// unaffected since they never depend on the facts index.
//
// A non-zero indexer exit is treated as fatal only if root has no usable
// database at all: per internal/index.Build's contract, the indexer
// itself now exits non-zero solely for conditions that leave its output
// untrustworthy (a failed graph load, a failed database open, a panic),
// never for an individual package's own parse/type-check failure. If a
// database from an earlier successful build still exists on disk, it is
// opened anyway — stale or incomplete is strictly better than
// unavailable — with a warning that it may not reflect this run.
// spawnIndexer starts exe — always this same running golance binary's own
// resolved path (see os.Executable, buildIndexLocked's only caller) — as
// the indexer subprocess, bound to ctx: canceling ctx (see
// Server.idxMu's doc and rpc.Server.Context) terminates it instead of
// leaving it to outlive the server process. exe is a function parameter,
// so gosec's subprocess-launched-with-variable check exempts it as the
// executable-name position (its own rule carves out parameters/receivers
// used there).
func spawnIndexer(ctx context.Context, exe string) *exec.Cmd {
	return exec.CommandContext(ctx, exe)
}

// buildIndex acquires s.idxMu (see its doc) and runs buildIndexLocked.
// This is the entry point for a cold-start build (no warm-opened index to
// revalidate); revalidateIndex, which already holds idxMu itself, calls
// buildIndexLocked directly instead.
func (s *Server) buildIndex(ctx context.Context, root string) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	s.buildIndexLocked(ctx, root)
}

// buildIndexLocked runs one indexer subprocess build against s.dbPath(root)
// (the shared path in the ordinary case) and installs its result. If that
// attempt finds the target database locked by another live session
// (runIndexBuild's locked return), it switches this session to its own
// private index (switchToPrivateIndex) and retries exactly once, now
// against the private path — covering not only the common case (tryWarmOpen
// already detected the lock before ever getting here, so s.dbPath(root)
// already resolves to the private path on this very first call) but also
// two sessions racing a cold start against the same not-yet-existing shared
// database at once, which tryWarmOpen alone cannot catch (see its doc).
func (s *Server) buildIndexLocked(ctx context.Context, root string) {
	dbPath := s.dbPath(root)
	if !s.runIndexBuild(ctx, root, dbPath) {
		return
	}
	s.switchToPrivateIndex()
	s.runIndexBuild(ctx, root, s.dbPath(root))
}

// runIndexBuild launches the indexer subprocess targeting dbPath, relays
// its progress, waits for it, and installs the result via
// openIndexAfterBuild. It reports locked=true when dbPath turned out to be
// held by another live session — the only outcome buildIndexLocked's retry
// reacts to; every other failure (reported via warnIndexUnavailable inside)
// is left as-is; there is nothing a different path would fix.
func (s *Server) runIndexBuild(ctx context.Context, root, dbPath string) (locked bool) {
	cas := casDir(root)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		s.warnIndexUnavailable(fmt.Sprintf("create index directory: %v", err))
		return false
	}

	exe, err := os.Executable()
	if err != nil {
		s.warnIndexUnavailable(fmt.Sprintf("resolve golance executable: %v", err))
		return false
	}

	cmd := spawnIndexer(ctx, exe)
	cmd.Env = append(os.Environ(),
		EnvIndexer+"=1",
		EnvRoot+"="+root,
		EnvDB+"="+dbPath,
		EnvCAS+"="+cas,
		EnvDepCAS+"="+depExportCASDir(),
		fmt.Sprintf("GOMAXPROCS=%d", max(1, runtime.NumCPU()-1)),
	)
	if s.opts.IndexJobs > 0 {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%d", EnvIndexJobs, s.opts.IndexJobs))
	}
	if s.opts.Offline {
		cmd.Env = append(cmd.Env, EnvOffline+"=1")
	}
	if s.opts.MemLimit != "" {
		cmd.Env = append(cmd.Env, "GOMEMLIMIT="+s.opts.MemLimit)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.warnIndexUnavailable(fmt.Sprintf("start indexer: %v", err))
		return false
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		s.warnIndexUnavailable(fmt.Sprintf("start indexer: %v", err))
		return false
	}

	done := make(chan struct{})
	var statsErrors int
	var began bool
	var summary string
	go func() {
		defer close(done)
		statsErrors, began, summary = s.relayIndexProgress(stdout)
	}()

	waitErr := cmd.Wait()
	<-done
	// openIndexAfterBuild (which installs s.idx) runs to completion BEFORE
	// the $/progress "end" notification goes out, not after: a client that
	// treats "end" as "the index is now queryable" — e.g. an E2E test's own
	// waitForIndexReady, or a real editor firing a hover the instant it
	// sees this notification — must never observe "end" while s.idx is
	// still nil. See notifyIndexProgressEnd's own doc for the race this
	// closes.
	locked = s.openIndexAfterBuild(ctx, dbPath, waitErr, stderr.String(), statsErrors)
	s.notifyIndexProgressEnd(began, summary)
	return locked
}

// openIndexAfterBuild opens dbPath and this session's CAS directory and
// installs them as the server's facts index, once the indexer subprocess
// that was building them has exited. On success, it also reindexes any
// package handleDidSave recorded dirty while no index was available (see
// drainDirty), so a save that landed during that window is not lost.
//
// waitErr is the subprocess's exit error (nil on success). If waitErr is
// non-nil and dbPath does not exist at all, this reports the original
// failure via window/showMessage and gives up: nothing was ever
// successfully indexed for this root, so there is no database to fall
// back to. Otherwise it attempts to open dbPath anyway — success, or a
// failure with a database already on disk from an earlier run, stale or
// incomplete being strictly better than unavailable. stderrText is the
// subprocess's full captured stderr (never truncated — internal/server
// buffers the whole thing via bytes.Buffer), included in the failure
// report and, when non-empty on an otherwise clean exit, logged as its own
// block (see below): per internal/index.Build's contract a non-zero exit
// code is reserved for conditions that leave the whole build untrustworthy,
// so a clean exit's stderr would otherwise never surface anywhere, hiding a
// panic or unexpected diagnostic from an individual package that still
// happened to leave the database usable overall. statsErrors is the
// indexer's own "STATS ... errors=N" count (see indexStatsMessage), logged
// as a visible warning whenever N > 0 regardless of exit code, since a
// per-package parse/type-check failure never changes the exit code either
// (see runIndexBuild) and the $/progress "end" notification's Message many
// clients simply ignore.
//
// It reports locked=true when the only reason dbPath could not be opened
// is that another live session currently holds its exclusive lock (see
// store.IsLocked) — checked before the "stale index" warning below fires,
// so that warning is never shown for a build this session is about to
// discard and retry against a private path instead (see
// buildIndexLocked). runIndexBuild's caller uses this to retry once
// against a session-private path (see switchToPrivateIndex) instead of
// leaving the facts index unavailable the way an ordinary open failure
// does.
func (s *Server) openIndexAfterBuild(ctx context.Context, dbPath string, waitErr error, stderrText string, statsErrors int) (locked bool) {
	stderrText = strings.TrimSpace(stderrText)
	if waitErr != nil {
		if _, statErr := os.Stat(dbPath); statErr != nil {
			s.warnIndexUnavailable(fmt.Sprintf("build index: %v (%s)", waitErr, stderrText))
			return false
		}
	}

	db, err := store.Open(dbPath)
	if err != nil {
		if store.IsLocked(err) {
			return true
		}
		s.warnIndexUnavailable(fmt.Sprintf("open index: %v", err))
		return false
	}

	if waitErr != nil {
		s.logger.Printf("golance: indexer exited with an error (%v: %s); opening the existing index, which may be stale or incomplete", waitErr, stderrText)
		s.showMessage(protocol.MessageTypeWarning, "golance: index build failed; opening the previous index, which may be stale or incomplete")
	} else if stderrText != "" {
		s.logIndexerStderr(stderrText)
	}
	if statsErrors > 0 {
		s.logger.Printf("golance: indexer reported %d package error(s) during this build; see the indexer stderr block for detail", statsErrors)
	}

	ws := s.workspace()
	if ws == nil {
		_ = db.Close()
		return false
	}
	cas, err := store.OpenCAS(casDir(ws.root))
	if err != nil {
		s.warnIndexUnavailable(fmt.Sprintf("open CAS: %v", err))
		_ = db.Close()
		return false
	}
	s.idx.Store(&indexState{db: db, cas: cas, resolver: s.newResolver(db, cas, ws.snap, RelativeIndexPaths(ws.root))})
	// Reset both one-time notice flags now that the index is genuinely
	// queryable again: indexUnavailableError and resolverOrWarn's own
	// logMessage key their wording off indexFailedWarned/indexBuildingWarned,
	// which — left permanently true from an earlier failed or in-flight
	// build — would otherwise keep describing a now-stale state (e.g. still
	// claiming "failed to build" long after a later revalidateIndex rebuild
	// succeeded) the next time s.idx goes nil again.
	s.indexBuildingWarned.Store(false)
	s.indexFailedWarned.Store(false)
	s.logger.Printf("golance: workspace index is now ready")
	s.drainDirty(ctx, ws)
	return false
}

// logIndexerStderr logs stderrText — the indexer subprocess's full captured
// stderr — as one clearly-prefixed block, so a long stderr (a panic trace,
// several packages' worth of parse/type-check diagnostics) reads as one
// attributable unit rather than blending into the surrounding log as an
// unexplained error storm. Called only for a clean exit with non-empty
// stderr (see openIndexAfterBuild); a non-zero exit already surfaces its
// stderr inline with the exit error itself.
func (s *Server) logIndexerStderr(stderrText string) {
	s.logger.Printf("golance: indexer stderr (build otherwise succeeded):\n%s", stderrText)
}

// closePrivateIndex closes and removes this session's own private facts
// index database (see switchToPrivateIndex/privateIndexDBFile), if this
// session ever built one — a no-op otherwise, and it never touches the
// shared index, which other live sessions may still be using. Called from
// Stop, after Serve has returned and drained every in-flight query (see
// Stop's own doc), so closing s.idx's db handle here cannot race a
// concurrent reader.
func (s *Server) closePrivateIndex() {
	if !s.usePrivateIndex.Load() {
		return
	}
	if idx := s.idx.Load(); idx != nil {
		if err := idx.db.Close(); err != nil {
			s.logger.Printf("golance: close private index: %v", err)
		}
	}
	ws := s.workspace()
	if ws == nil {
		return
	}
	path := privateIndexDBFile(ws.root, s.sessionID)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		s.logger.Printf("golance: remove private index %s: %v", path, err)
	}
}

// cleanupOrphanedPrivateIndexes removes stale session-private index
// database files (see privateIndexDBFile) left behind by a session that
// crashed, or was killed, before its own Stop could remove them: a private
// file is bound to exactly one session's lifetime, so nothing else should
// ever hold its lock once its owning process is gone. It walks root's
// cache directory for files matching the private-suffix pattern
// (privateIndexGlobPattern — the shared index file itself is never
// matched) and removes any whose bbolt lock can be acquired immediately
// (store.TryClaimAbandoned): a file still genuinely in use by another live
// session fails that probe and is left untouched, exactly like the shared
// file always is.
//
// This is opportunistic best-effort housekeeping, not required for
// correctness (an orphan left behind is otherwise harmless — it is simply
// never opened by anything again), so callers run it via s.rpc.Go in the
// background rather than blocking "initialize" on it.
func (s *Server) cleanupOrphanedPrivateIndexes(root string) {
	ownPath := privateIndexDBFile(root, s.sessionID)
	matches, err := filepath.Glob(privateIndexGlobPattern(root))
	if err != nil {
		s.logger.Printf("golance: glob orphaned private indexes: %v", err)
		return
	}
	for _, path := range matches {
		if path == ownPath {
			continue // this session's own private index, still in use
		}
		if store.TryClaimAbandoned(path) {
			s.logger.Printf("golance: removed orphaned private index %s", path)
		}
	}
}

func (s *Server) warnIndexUnavailable(detail string) {
	if s.indexFailedWarned.CompareAndSwap(false, true) {
		s.showMessage(protocol.MessageTypeWarning, "golance: "+detail+"; cross-reference features are unavailable until the next successful index build")
		return
	}
	s.logger.Printf("golance: %s", detail)
}

func (s *Server) showMessage(typ protocol.MessageType, msg string) {
	err := s.rpc.Notify(protocol.MethodWindowShowMessage, &protocol.ShowMessageParams{Type: typ, Message: msg})
	if err != nil {
		s.logger.Printf("server: show message: %v", err)
	}
}

// logMessage sends msg to the client via window/logMessage: informational
// detail for the editor's own log/output panel, never a popup. Some clients
// render window/showMessage as a modal the user must dismiss (e.g. a
// blocking "press ENTER" prompt in a terminal-based editor) — reserve
// showMessage for failures that genuinely need the user's attention (see
// warnIndexUnavailable) and use logMessage for everything else, including
// routine "index still building" notices. Always MessageTypeInfo (every
// call site wants exactly that — logMessage is reserved for routine,
// non-actionable notices; anything severe enough to warrant Warning or
// Error belongs in showMessage instead), so unlike showMessage this takes
// no MessageType parameter at all.
func (s *Server) logMessage(msg string) {
	err := s.rpc.Notify(protocol.MethodWindowLogMessage, &protocol.LogMessageParams{Type: protocol.MessageTypeInfo, Message: msg})
	if err != nil {
		s.logger.Printf("server: log message: %v", err)
	}
}

// progressPercent returns done/total as a percentage in [0, 100], or 0 if
// total is not yet known (<= 0) or either value is out of the range this
// computation can trust — "PROGRESS done total" lines come from golance's
// own indexer subprocess (see relayIndexProgress), so this is defensive
// against a malformed line, not untrusted external input.
func progressPercent(done, total int) uint32 {
	if total <= 0 {
		return 0
	}
	if done < 0 {
		return 0
	}
	p := done * 100 / total
	if p < 0 {
		return 0
	}
	if p > math.MaxUint32 {
		return 0
	}
	return uint32(p)
}

// indexProgressToken is the $/progress token relayIndexProgress and
// notifyIndexProgressEnd report the indexer subprocess's build progress
// under.
const indexProgressToken = "golance/index"

// relayIndexProgress reads "PROGRESS done total" lines written by the
// indexer subprocess's stdout (see cmd/golance's indexer entry point) and
// relays them as $/progress "begin"/"report" notifications. It deliberately
// does NOT send the matching "end" notification itself — see
// notifyIndexProgressEnd, which runIndexBuild calls once openIndexAfterBuild
// has actually installed (or failed to install) the result, and why that
// ordering matters.
//
// The subprocess's final "STATS ..." summary line (see indexStatsMessage)
// is returned as summary, to become the "end" notification's own Message
// once notifyIndexProgressEnd sends it, so a client — including the E2E
// suite, which asserts on it directly instead of on wall-clock build time —
// can tell how many packages this build actually type-checked versus
// resolved via a CAS hit or an unchanged-content skip. statsErrors is that
// line's own errors=N count, so runIndexBuild's caller can additionally log
// a warning many clients would otherwise never surface (see
// openIndexAfterBuild) instead of relying solely on the "end" message,
// which many clients ignore. began reports whether any progress was ever
// relayed at all — notifyIndexProgressEnd's own "was there a 'begin' to
// match" gate, since a "PROGRESS" line only appears once actual per-package
// work starts.
//
// This does not implement the full window/workDoneProgress/create
// handshake: internal/rpc.Server has no mechanism for a server-initiated
// outbound request awaiting a client response, so indexProgressToken is
// sent unsolicited rather than created first. Clients that strictly
// require a create round-trip before accepting $/progress will ignore
// these notifications; this is a known v0.1 limitation of the transport
// layer, not a bug in the relay itself.
func (s *Server) relayIndexProgress(r io.Reader) (statsErrors int, began bool, summary string) {
	// maxProgressLine bounds a single line of the subprocess's stdout.
	// "PROGRESS %d %d" and "STATS ..." lines are always a few dozen bytes;
	// this is a generous, explicit safety margin rather than
	// bufio.NewScanner's unstated default (bufio.MaxScanTokenSize), so a
	// malformed or oversized line fails loudly (see the sc.Err() handling
	// below) instead of silently relying on a package-internal constant.
	const maxProgressLine = 1 << 20 // 1 MiB

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), maxProgressLine)
	for sc.Scan() {
		line := sc.Text()
		var done, total int
		if _, err := fmt.Sscanf(line, "PROGRESS %d %d", &done, &total); err == nil {
			if !began {
				began = true
				s.notifyProgress(indexProgressToken, &protocol.WorkDoneProgressBegin{Kind: "begin", Title: "golance: building index"})
			}
			pct := progressPercent(done, total)
			msg := fmt.Sprintf("%d/%d packages", done, total)
			s.notifyProgress(indexProgressToken, &protocol.WorkDoneProgressReport{Kind: "report", Percentage: &pct, Message: &msg})
			continue
		}
		if msg, errs, ok := indexStatsMessage(line); ok {
			summary = msg
			statsErrors = errs
		}
	}
	if err := sc.Err(); err != nil {
		// A read failure here means the progress stream was cut short, so
		// summary and statsErrors reflect only what arrived before it: the
		// build may have reported failures this relay never saw. Called out
		// by name when the cause is a line past maxProgressLine, since that
		// case specifically means the final STATS line — and so
		// statsErrors's count — was likely never reached.
		if errors.Is(err, bufio.ErrTooLong) {
			s.logger.Printf("golance: indexer progress line exceeded %d bytes, rest of the progress stream (including the final STATS summary) was dropped", maxProgressLine)
		} else {
			s.logger.Printf("golance: read indexer progress: %v", err)
		}
	}
	return statsErrors, began, summary
}

// notifyIndexProgressEnd sends the $/progress "end" notification matching
// relayIndexProgress's own "begin" (a no-op if began is false — nothing to
// end), with summary as its Message if non-empty.
//
// Callers must send this only once whatever this build's own progress
// stream reported is actually true of the server's state — in practice,
// only after openIndexAfterBuild has finished installing (or failing to
// install) s.idx. relayIndexProgress used to send "end" itself, the instant
// the indexer subprocess's stdout stream closed — which is always strictly
// BEFORE openIndexAfterBuild even starts, since runIndexBuild only calls it
// after both cmd.Wait() and the progress relay have returned. A client that
// treats "end" as "the index is now usable" (the E2E suite's own
// waitForIndexReady, and any real editor doing the equivalent) could then
// issue a cross-package hover, typeDefinition, or call/type hierarchy query
// that read s.idx while it was still nil — a successful, silently empty (or,
// for typeDefinition/call/type hierarchy, now correctly erroring) answer for
// a query that would have found something moments later, purely because of
// this ordering gap.
func (s *Server) notifyIndexProgressEnd(began bool, summary string) {
	if !began {
		return
	}
	end := &protocol.WorkDoneProgressEnd{Kind: "end"}
	if summary != "" {
		end.Message = &summary
	}
	s.notifyProgress(indexProgressToken, end)
}

// indexStatsMessage turns one "STATS processed=P skipped=S errors=E
// typechecked=T" line (see cmd/golance's indexer entry point) into a
// human-readable summary for the $/progress "end" notification's Message
// plus that line's own errors=N count, reporting ok=false for anything else
// relayIndexProgress reads off the subprocess's stdout.
func indexStatsMessage(line string) (msg string, errs int, ok bool) {
	var processed, skipped, typeChecked int
	if _, err := fmt.Sscanf(line, "STATS processed=%d skipped=%d errors=%d typechecked=%d", &processed, &skipped, &errs, &typeChecked); err != nil {
		return "", 0, false
	}
	casHits := processed - typeChecked
	return fmt.Sprintf("%d type-checked, %d resolved from cache, %d unchanged, %d error(s)", typeChecked, casHits, skipped, errs), errs, true
}

func (s *Server) notifyProgress(token string, value any) {
	b, err := protocol.Marshal(value)
	if err != nil {
		s.logger.Printf("server: marshal progress value: %v", err)
		return
	}
	err = s.rpc.Notify(protocol.MethodProgress, &protocol.ProgressParams{
		Token: protocol.String(token),
		Value: protocol.LSPAny(b),
	})
	if err != nil {
		s.logger.Printf("server: notify progress: %v", err)
	}
}
