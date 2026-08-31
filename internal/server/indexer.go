package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"go.lsp.dev/protocol"

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
func repoKey(root string) (key string, shared bool) {
	cmd := exec.Command("git", "rev-parse", "--git-common-dir")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return root, false
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return root, false
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return root, false
	}
	// Resolve symlinks so every worktree's key lands on the same string
	// regardless of which absolute form its own root happened to be given
	// in: git already returns a symlink-resolved absolute path for a linked
	// worktree's --git-common-dir (it resolves the worktree's `.git` file
	// target internally), but the main worktree's own relative "./.git"
	// answer, joined above onto a possibly-unresolved root, would not
	// match that without this — e.g. macOS's /var -> /private/var.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs, true
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

// indexDBFile returns the per-root index database path for root: the small
// bbolt database mapping each package to its current CAS blob key plus the
// name/method/SymbolID-string lookup indices (see the internal/store
// package doc). Unlike the pre-redesign single shared database, this is
// always private to root — never shared with another worktree, even of the
// same repository — so two golance sessions for different worktrees never
// contend for it and no lock or fallback path is ever needed.
func indexDBFile(root string) string {
	return cacheDBFile("index", root)
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
// root (see indexDBFile) — a pure function of root, computed fresh on every
// call: unlike the pre-redesign design, there is no shared-file contention
// to probe for, so there is nothing to decide once and remember.
func (s *Server) dbPath(root string) string { return indexDBFile(root) }

// tryWarmOpen opens root's existing per-root index database and CAS
// directly, if the database exists, without waiting for any revalidation.
// This lets cross-reference queries answer immediately on a second-or-later
// session instead of waiting for a rebuild pass that — CAS-hit fast paths
// aside — still has to enumerate every workspace package before confirming
// nothing changed. It reports ok=false if no database exists yet, in which
// case the caller should fall back to buildIndex.
//
// The database opened here may be stale — built under a different
// toolchain, or missing changes made outside this session since it was
// last built — since this does not check the build fingerprint or
// otherwise revalidate anything. handleInitialize pairs a successful
// warm-open with revalidateIndex, which checks in the background and
// triggers a rebuild if it finds anything stale; until that finishes,
// queries simply answer from whatever this opened.
func (s *Server) tryWarmOpen(root string) (*indexState, bool) {
	dbPath := s.dbPath(root)
	if _, err := os.Stat(dbPath); err != nil {
		return nil, false
	}
	db, err := store.Open(dbPath)
	if err != nil {
		s.logger.Printf("golance: warm-open index: %v", err)
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
	return &indexState{db: db, cas: cas, resolver: xref.New(db, cas, ws.snap, RelativeIndexPaths(root))}, true
}

// revalidateIndex checks, cheaply and in-process, whether root's
// warm-opened facts index (installed by a prior tryWarmOpen) is still up
// to date — the same skip logic [index.Build] uses to decide whether a
// package needs rechecking, minus any type-checking itself (see
// index.Revalidate). This runs concurrently with query handling and any
// in-session Reindex against the same *store.DB (bbolt supports any number
// of concurrent readers alongside one writer on one open handle), so
// nothing needs to pause while it runs.
//
// If nothing is stale, this is a no-op: the warm-opened index keeps
// serving as-is. Otherwise — a toolchain change, or a file changed outside
// this session since it was last built — it closes the warm-opened db
// handle (releasing it; bbolt's Close blocks until any in-flight read
// finishes, so this does not race a concurrent query) and falls back to
// buildIndex, the same full-rebuild path a cold start (no warm-open at
// all) uses.
//
// v0.1 scope: this check runs once, shortly after initialize. A pull that
// lands after it has already run is not picked up until the next restart
// (or a go.mod/go.sum/go.work change, which handleDidChangeWatchedFiles
// separately reloads the import graph for) — there is no ongoing poll for
// external file changes during the rest of the session.
func (s *Server) revalidateIndex(root string) {
	if !s.indexNeedsRebuild() {
		return
	}
	idx := s.idx.Load()
	s.idx.Store(nil)
	if err := idx.db.Close(); err != nil {
		s.logger.Printf("golance: close index before rebuild: %v", err)
	}
	s.buildIndex(root)
}

// indexNeedsRebuild reports whether the currently warm-opened index (if
// any) is stale, per index.Revalidate. False whenever there is nothing
// warm-opened to check, or the check itself fails (conservatively: keep
// serving what is already open rather than force a rebuild on every
// transient error).
func (s *Server) indexNeedsRebuild() bool {
	idx := s.idx.Load()
	if idx == nil {
		return false
	}
	ws := s.workspace()
	if ws == nil {
		return false
	}
	changed, err := index.Revalidate(context.Background(), ws.snap, idx.db, runtime.Version(), "", RelativeIndexPaths(ws.root))
	if err != nil {
		s.logger.Printf("golance: revalidate index: %v", err)
		return false
	}
	return changed
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
// resolved path (see os.Executable, buildIndex's only caller) — as the
// indexer subprocess. exe is a function parameter, so gosec's
// subprocess-launched-with-variable check exempts it as the
// executable-name position (its own rule carves out parameters/receivers
// used there).
func spawnIndexer(exe string) *exec.Cmd {
	return exec.Command(exe)
}

func (s *Server) buildIndex(root string) {
	dbPath := s.dbPath(root)
	cas := casDir(root)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		s.warnIndexUnavailable(fmt.Sprintf("create index directory: %v", err))
		return
	}

	exe, err := os.Executable()
	if err != nil {
		s.warnIndexUnavailable(fmt.Sprintf("resolve golance executable: %v", err))
		return
	}

	cmd := spawnIndexer(exe)
	cmd.Env = append(os.Environ(),
		EnvIndexer+"=1",
		EnvRoot+"="+root,
		EnvDB+"="+dbPath,
		EnvCAS+"="+cas,
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
		return
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		s.warnIndexUnavailable(fmt.Sprintf("start indexer: %v", err))
		return
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.relayIndexProgress(stdout)
	}()

	waitErr := cmd.Wait()
	<-done
	s.openIndexAfterBuild(dbPath, waitErr, stderr.String())
}

// openIndexAfterBuild opens dbPath and this session's CAS directory and
// installs them as the server's facts index, once the indexer subprocess
// that was building them has exited.
//
// waitErr is the subprocess's exit error (nil on success). If waitErr is
// non-nil and dbPath does not exist at all, this reports the original
// failure via window/showMessage and gives up: nothing was ever
// successfully indexed for this root, so there is no database to fall
// back to. Otherwise — success, or a failure with a database already on
// disk from an earlier run — it opens dbPath anyway, warning (on failure)
// that the result may be stale or incomplete. stderrText is the
// subprocess's captured stderr, included in the failure report.
func (s *Server) openIndexAfterBuild(dbPath string, waitErr error, stderrText string) {
	if waitErr != nil {
		if _, statErr := os.Stat(dbPath); statErr != nil {
			s.warnIndexUnavailable(fmt.Sprintf("build index: %v (%s)", waitErr, strings.TrimSpace(stderrText)))
			return
		}
		s.logger.Printf("golance: indexer exited with an error (%v: %s); opening the existing index, which may be stale or incomplete", waitErr, strings.TrimSpace(stderrText))
		s.showMessage(protocol.MessageTypeWarning, "golance: index build failed; opening the previous index, which may be stale or incomplete")
	}

	db, err := store.Open(dbPath)
	if err != nil {
		s.warnIndexUnavailable(fmt.Sprintf("open index: %v", err))
		return
	}
	ws := s.workspace()
	if ws == nil {
		_ = db.Close()
		return
	}
	cas, err := store.OpenCAS(casDir(ws.root))
	if err != nil {
		s.warnIndexUnavailable(fmt.Sprintf("open CAS: %v", err))
		_ = db.Close()
		return
	}
	s.idx.Store(&indexState{db: db, cas: cas, resolver: xref.New(db, cas, ws.snap, RelativeIndexPaths(ws.root))})
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
// routine "index still building" notices.
func (s *Server) logMessage(typ protocol.MessageType, msg string) {
	err := s.rpc.Notify(protocol.MethodWindowLogMessage, &protocol.LogMessageParams{Type: typ, Message: msg})
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

// relayIndexProgress reads "PROGRESS done total" lines written by the
// indexer subprocess's stdout (see cmd/golance's indexer entry point) and
// relays them as $/progress notifications. The subprocess's final "STATS
// ..." summary line (see indexStatsMessage) becomes the "end" notification's
// Message, so a client — including the E2E suite, which asserts on it
// directly instead of on wall-clock build time — can tell how many
// packages this build actually type-checked versus resolved via a CAS hit
// or an unchanged-content skip.
//
// This does not implement the full window/workDoneProgress/create
// handshake: internal/rpc.Server has no mechanism for a server-initiated
// outbound request awaiting a client response, so the progress token below
// is sent unsolicited rather than created first. Clients that strictly
// require a create round-trip before accepting $/progress will ignore
// these notifications; this is a known v0.1 limitation of the transport
// layer, not a bug in the relay itself.
func (s *Server) relayIndexProgress(r io.Reader) {
	const token = "golance/index"
	began := false
	var summary string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		var done, total int
		if _, err := fmt.Sscanf(line, "PROGRESS %d %d", &done, &total); err == nil {
			if !began {
				began = true
				s.notifyProgress(token, &protocol.WorkDoneProgressBegin{Kind: "begin", Title: "golance: building index"})
			}
			pct := progressPercent(done, total)
			msg := fmt.Sprintf("%d/%d packages", done, total)
			s.notifyProgress(token, &protocol.WorkDoneProgressReport{Kind: "report", Percentage: &pct, Message: &msg})
			continue
		}
		if msg, ok := indexStatsMessage(line); ok {
			summary = msg
		}
	}
	if began {
		end := &protocol.WorkDoneProgressEnd{Kind: "end"}
		if summary != "" {
			end.Message = &summary
		}
		s.notifyProgress(token, end)
	}
}

// indexStatsMessage turns one "STATS processed=P skipped=S errors=E
// typechecked=T" line (see cmd/golance's indexer entry point) into a
// human-readable summary for the $/progress "end" notification's Message,
// reporting ok=false for anything else relayIndexProgress reads off the
// subprocess's stdout.
func indexStatsMessage(line string) (msg string, ok bool) {
	var processed, skipped, errs, typeChecked int
	if _, err := fmt.Sscanf(line, "STATS processed=%d skipped=%d errors=%d typechecked=%d", &processed, &skipped, &errs, &typeChecked); err != nil {
		return "", false
	}
	casHits := processed - typeChecked
	return fmt.Sprintf("%d type-checked, %d resolved from cache, %d unchanged, %d error(s)", typeChecked, casHits, skipped, errs), true
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
