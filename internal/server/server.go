// Package server wires golance's independent internal packages (rpc,
// overlay, graph, check, langfeat, xref, index, store) into a running LSP
// server: handler registration, coordinate conversion between the LSP wire
// protocol and each package's own coordinate system, and process lifecycle
// (workspace load, indexer subprocess, incremental reindex).
package server

import (
	"context"
	"encoding/json"
	"log"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"go.lsp.dev/protocol"

	golance "github.com/sivchari/golance"
	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/depcheck"
	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/xref"
)

// Version is golance's user-visible version string, reported in
// InitializeResult.ServerInfo and by cmd/golance's -version flag. The
// canonical value lives in the repo-root version.go, which tagpr bumps on
// release.
const Version = golance.Version

// Options configures a Server.
type Options struct {
	// Logger receives diagnostic log lines. Defaults to log.Default().
	Logger *log.Logger
	// IndexJobs bounds index build parallelism (index.Options.Parallelism)
	// in the indexer subprocess. 0 uses index's own default.
	IndexJobs int
	// MemLimit is forwarded to the indexer subprocess as GOMEMLIMIT. Empty
	// leaves the Go runtime's default memory limit in place.
	MemLimit string
	// Offline forbids the initial graph load and the indexer subprocess
	// from downloading modules (GOPROXY=off).
	Offline bool
	// WatchDebounce is how long handleDidChangeWatchedFiles waits for .go
	// file-change notifications to go quiet before revalidating the
	// workspace (see watchDebouncer). <= 0 uses defaultWatchDebounce.
	WatchDebounce time.Duration
}

// workspace bundles every value that depends on the current import graph
// snapshot, so a workspace/didChangeWatchedFiles-triggered reload can swap
// them in atomically without a lock on the read path.
type workspace struct {
	root     string
	snap     *graph.Snapshot
	engine   *check.Engine
	depCache *depCacheHolder
	// depProvider resolves non-workspace (standard library, module
	// dependency, test-only) packages by type-checking their own real
	// source on demand (internal/depcheck), for navigation consumers that
	// need an exact declaration position or unexported visibility into a
	// dependency — currently only langfeat.DependencyDefinition (see
	// dependencyDefinition in handlers_xref.go). Unlike depCache, this
	// Provider is a server-lifetime value setWorkspace installs by pointer
	// (see (*Server).ensureDepProvider): it is only rebuilt from scratch
	// when the workspace's dependency set has actually changed, and simply
	// retargeted at the new snapshot otherwise, so its type-check cache
	// survives an ordinary workspace edit's setWorkspace call instead of
	// being discarded and rebuilt cold every time (see ensureDepProvider's
	// own doc for the production stall this fixes).
	depProvider *depcheck.Provider
	fileToPkg   map[string]string
	// dirToPkg maps a package's directory to its import path, the fallback
	// pkgPathForFile uses for a file fileToPkg does not itself know about —
	// in particular an in-package _test.go file, which graph.Package.GoFiles
	// never includes (see internal/graph's loadMode). Mirrors
	// internal/check.GraphSource.PackageForFile's identical fallback, built
	// from the same snap.Packages.
	dirToPkg map[string]string
	// pkgNameIndex maps a graph-known package's declared name (e.g.
	// "strings") to the sorted import paths of every package sharing that
	// name — the candidate source for unimported-package completion (see
	// unimportedPackageCandidates/unimportedMemberCandidates). Built once
	// per snapshot from snap.Packages' already-loaded Name field (free —
	// see graph.Package.Name's doc), never touched again until the next
	// setWorkspace, so a completion request never walks the graph itself.
	pkgNameIndex map[string][]string
}

// indexState bundles the per-root index database, the CAS its facts and
// export data live in, and the Resolver over both. It is nil until the
// indexer subprocess completes a build successfully.
type indexState struct {
	db       *store.DB
	cas      *store.CAS
	resolver *xref.Resolver
}

// Server wires golance's independent internal packages into a running LSP
// server. Construct with New, which registers every handler on rpcServer;
// call rpcServer.Serve to run it.
type Server struct {
	opts    Options
	logger  *log.Logger
	rpc     *rpc.Server
	overlay *overlay.Overlay

	ws    atomic.Pointer[workspace]
	idx   atomic.Pointer[indexState]
	hints atomic.Pointer[map[langfeat.HintKind]bool] // enabled inlay hint kinds; nil until "initialize" or workspace/didChangeConfiguration sets it, meaning every kind enabled (see hintsEnabled)

	// depProviderMu guards depProviderKey/depProviderSrc/depProviderVal — the
	// server-lifetime depcheck.Provider setWorkspace installs into each new
	// workspace's own depProvider field (see ensureDepProvider). Kept here,
	// not per-workspace, precisely so it can OUTLIVE a workspace: its whole
	// reason to exist is surviving a setWorkspace swap that leaves the
	// dependency set (see depsKey) unchanged.
	depProviderMu  sync.Mutex
	depProviderKey string
	depProviderSrc *depMetadataSource
	depProviderVal *depcheck.Provider

	// idxMu serializes every revalidateIndex/buildIndex invocation: without
	// it, the once-per-session post-initialize background check
	// (lifecycle.go) and a watched-files-triggered revalidateWorkspace pass
	// (workspace.go) can both observe s.idx as "stale, not yet nil'd" and
	// race on closing/rebuilding it concurrently — a nil-pointer panic in
	// the interleaving where one goroutine's Store(nil) lands between the
	// other's own Load and Close, redundant indexer subprocesses otherwise,
	// and no guarantee the slower rebuild doesn't overwrite a fresher one's
	// result. Holding it across an entire rebuild (including the indexer
	// subprocess's exit) also means at most one indexer subprocess ever
	// runs at a time.
	idxMu sync.Mutex

	diagMu sync.Mutex
	// diagFiles is keyed by check.Result.PkgPath, not directory: a directory
	// can hold two independent units (its base package and, separately, its
	// external "_test" package — see internal/check's unitKey), each
	// publishing its own Result, so tracking per-directory would let one
	// unit's publish clear the other's not-yet-republished diagnostics (see
	// publishDiagnostics's doc).
	diagFiles map[string]map[string]bool // check.Result.PkgPath -> files last published with diagnostics

	indexBuildingWarned atomic.Bool // one-time window/logMessage while the index is still building
	indexFailedWarned   atomic.Bool // one-time window/showMessage after an indexer failure

	watch           *watchDebouncer    // coalesces workspace/didChangeWatchedFiles .go events into revalidateWorkspace passes
	watchFP         *watchFingerprints // recognizes and suppresses a no-op watched-files event before it reaches s.watch (see watchFingerprints)
	watchDynamicReg atomic.Bool        // client declared workspace.didChangeWatchedFiles.dynamicRegistration support at initialize (see handleInitialized)

	inlayHintRefreshSupport      atomic.Bool // client declared workspace.inlayHint.refreshSupport at initialize (see refreshInlayHints)
	semanticTokensRefreshSupport atomic.Bool // client declared workspace.semanticTokens.refreshSupport at initialize (see refreshSemanticTokens)

	// clientInitialized reports whether the client's "initialized"
	// notification has been received (set by handleInitialized). The LSP
	// spec forbids any server-initiated request before that point.
	// setWorkspace's own workspace-ready refresh (see
	// workspaceReadyRefreshes) needs this explicit check because its very
	// first call happens synchronously inside handleInitialize — well
	// before the client can have sent "initialized" — unlike
	// registerWatchedFiles, which is safe by construction simply because
	// handleInitialized is its only caller.
	clientInitialized atomic.Bool

	// sessionID uniquely identifies this Server instance (not just this
	// process — see newSessionID's doc), embedded in this session's own
	// private index database filename (see privateIndexDBFile) once
	// usePrivateIndex is set.
	sessionID string

	// usePrivateIndex reports whether this session has switched from the
	// shared per-root facts index (indexDBFile) to a session-private one
	// (privateIndexDBFile) after finding the shared database locked by
	// another live session (see switchToPrivateIndex). Sticky for the rest
	// of the session once set: dbPath, and the file Stop removes, must
	// keep agreeing on the same path.
	usePrivateIndex atomic.Bool

	// dirtyMu guards dirtyPkgs.
	dirtyMu sync.Mutex
	// dirtyPkgs records package import paths handleDidSave saved while
	// s.idx was nil (index still building, or briefly swapped out mid
	// revalidateIndex), pending reindex once an index becomes available —
	// see markDirty/takeDirty/drainDirty. Without this, such a save was
	// simply lost until some unrelated later change happened to touch the
	// same package again.
	dirtyPkgs map[string]bool

	// wsReady is closed exactly once, by the first setWorkspace call to
	// install a workspace (see waitWorkspace) — signaling any blocked
	// caller that s.workspace() will now return non-nil. Relevant only
	// during the async window between handleInitialize returning and its
	// background loadWorkspaceAsync call finishing; an already-ready
	// session never observes it closed, since waitWorkspace checks
	// s.workspace() itself first.
	wsReady     chan struct{}
	wsReadyOnce sync.Once

	// pendingOpensMu guards pendingOpens.
	pendingOpensMu sync.Mutex
	// pendingOpens records document paths handleDidOpen saw while
	// s.workspace() was still nil (the async window right after
	// handleInitialize, before loadWorkspaceAsync's first setWorkspace call
	// completes), pending SetFocus/Invalidate once a workspace becomes
	// available — see markPendingOpen/takePendingOpens/drainPendingOpens.
	// Without this, a client's didOpen sent immediately after "initialized"
	// — a normal sequence, and now a much wider window than before this
	// window's load moved off the "initialize" response's own critical
	// path — would never have its buffer focused (exempted from the check
	// engine's LRU eviction) or its first diagnostics recheck scheduled,
	// until some unrelated later edit happened to touch the same package.
	// Mirrors dirtyPkgs' identical queue-until-available pattern for a
	// didSave landing while s.idx is nil.
	pendingOpens map[string]bool
}

// New constructs a Server and registers its LSP handlers on rpcServer.
// Handlers reject cross-reference and interactive requests gracefully
// until the "initialize" request populates the workspace.
func New(rpcServer *rpc.Server, opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{
		opts:      opts,
		logger:    logger,
		rpc:       rpcServer,
		overlay:   overlay.New(),
		diagFiles: make(map[string]map[string]bool),
		sessionID: newSessionID(),
		wsReady:   make(chan struct{}),
	}
	s.watch = newWatchDebouncer(opts.WatchDebounce, s.revalidateWorkspace)
	s.watchFP = newWatchFingerprints()
	s.wire()
	return s
}

// wire registers every LSP method this Server answers on s.rpc. Definition,
// references, implementation, workspace/symbol, and rename run on the
// Background pool (see internal/rpc.Priority) since they are workspace-wide
// queries; every other request is Interactive.
func (s *Server) wire() {
	s.rpc.Handle(protocol.MethodInitialize, rpc.Interactive, s.handleInitialize)
	s.rpc.Handle(protocol.MethodShutdown, rpc.Interactive, s.handleShutdown)

	s.rpc.HandleNotification(protocol.MethodInitialized, s.handleInitialized)
	s.rpc.HandleNotification(protocol.MethodTextDocumentDidOpen, s.handleDidOpen)
	s.rpc.HandleNotification(protocol.MethodTextDocumentDidChange, s.handleDidChange)
	s.rpc.HandleNotification(protocol.MethodTextDocumentDidSave, s.handleDidSave)
	s.rpc.HandleNotification(protocol.MethodTextDocumentDidClose, s.handleDidClose)
	s.rpc.HandleNotification(protocol.MethodWorkspaceDidChangeWatchedFiles, s.handleDidChangeWatchedFiles)
	s.rpc.HandleNotification(protocol.MethodWorkspaceDidChangeConfiguration, s.handleDidChangeConfiguration)

	s.rpc.Handle(protocol.MethodTextDocumentHover, rpc.Interactive, s.instrument(protocol.MethodTextDocumentHover, s.handleHover))
	s.rpc.Handle(protocol.MethodTextDocumentSignatureHelp, rpc.Interactive, s.handleSignatureHelp)
	s.rpc.Handle(protocol.MethodTextDocumentDocumentSymbol, rpc.Interactive, s.handleDocumentSymbol)
	s.rpc.Handle(protocol.MethodTextDocumentInlayHint, rpc.Interactive, s.handleInlayHint)
	s.rpc.Handle(protocol.MethodTextDocumentFormatting, rpc.Interactive, s.handleFormatting)
	s.registerCodeActionHandlers()
	s.registerSemanticHandlers()

	s.rpc.Handle(protocol.MethodTextDocumentDefinition, rpc.Background, s.instrument(protocol.MethodTextDocumentDefinition, s.handleDefinition))
	s.rpc.Handle(protocol.MethodTextDocumentReferences, rpc.Background, s.instrument(protocol.MethodTextDocumentReferences, s.handleReferences))
	s.rpc.Handle(protocol.MethodTextDocumentImplementation, rpc.Background, s.instrument(protocol.MethodTextDocumentImplementation, s.handleImplementation))
	s.rpc.Handle(protocol.MethodWorkspaceSymbol, rpc.Background, s.handleWorkspaceSymbol)
	s.rpc.Handle(protocol.MethodTextDocumentRename, rpc.Background, s.handleRename)
	s.registerNavHandlers()

	// prepareCallHierarchy/outgoingCalls type-check a single package (the
	// same checkedFile machinery hover/completion use), so they run
	// Interactive like those; incomingCalls instead answers from the
	// persisted reverse reference index (like references), so it runs
	// Background alongside it. See handlers_callhierarchy.go's doc.
	s.rpc.Handle(protocol.MethodTextDocumentPrepareCallHierarchy, rpc.Interactive, s.handlePrepareCallHierarchy)
	s.rpc.Handle(protocol.MethodCallHierarchyIncomingCalls, rpc.Background, s.handleIncomingCalls)
	s.rpc.Handle(protocol.MethodCallHierarchyOutgoingCalls, rpc.Interactive, s.handleOutgoingCalls)
}

// instrument wraps h so that, after it returns, a total duration at or past
// slowRequestThreshold is logged at INFO: method, duration, and — for a
// handler whose own call chain threads the request's phaseTimer through
// (see withPhaseTimer/phaseTimerFrom; currently handleDefinition,
// handleImplementation, handleReferences, and every resolveCheckedPackage
// caller, i.e. hover and the definition/type-definition fallback paths) —
// the single phase that took longest, e.g. "engine.Get" versus
// "depcheck.check" versus "facts". This exists so a field report of an
// intermittently slow navigation query ("worked fast at first, then
// stalled") can be diagnosed straight from a user's server log, without
// asking them to reproduce it under a profiler. handleReferences also
// installs pt as an internal/xref.StatsSink (see its own doc), so a slow
// References query's line additionally reports how many closure-walk units
// it visited and how many bytes/records that cost (see
// phaseTimer.countsSuffix) — "" for every other handler, unaffected. A
// request that finishes under the threshold costs one time.Since call and
// nothing else — no allocation, no log line.
func (s *Server) instrument(method string, h rpc.RequestHandler) rpc.RequestHandler {
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		ctx, pt := withPhaseTimer(ctx)
		start := time.Now()
		result, err := h(ctx, params)
		if d := time.Since(start); d >= slowRequestThreshold {
			counts := pt.countsSuffix()
			if phase, phaseDur := pt.dominant(); phase != "" {
				s.logger.Printf("golance: slow request: method=%s duration=%s dominant_phase=%s dominant_phase_duration=%s%s", method, d, phase, phaseDur, counts)
			} else {
				s.logger.Printf("golance: slow request: method=%s duration=%s%s", method, d, counts)
			}
		}
		return result, err
	}
}

// Stop releases session resources that outlive rpcServer.Serve itself:
// s.watch's own pending/in-flight debounce work (see watchDebouncer.Stop),
// and — if this session ever fell back to one (see switchToPrivateIndex) —
// this session's own private facts index database, closed and removed so
// it does not outlive the session (see closePrivateIndex). Background work
// launched via s.rpc.Go (the indexer subprocess, a didSave-triggered
// reindex) already stops on its own once Serve cancels its context, and is
// drained by Serve's own wg.Wait before Serve returns, so by the time Stop
// runs nothing is still reading s.idx concurrently. Callers should call
// this after Serve returns.
func (s *Server) Stop() {
	s.watch.Stop()
	s.closePrivateIndex()
}

// workspace returns the current workspace bundle, or nil before the
// "initialize" request has completed.
func (s *Server) workspace() *workspace { return s.ws.Load() }

// waitWorkspace returns the current workspace, blocking until one becomes
// available or ctx is done, whichever comes first, if none is loaded yet.
// checkedFile — the resolution chokepoint behind hover, completion,
// signature help, and textDocument/definition's index-unavailable fallback
// — uses this instead of workspace() so a request arriving during
// golance's async initial graph load (see handleInitialize/
// loadWorkspaceAsync) gets a real answer once that finishes, within
// whatever the client's own ctx allows (its request timeout, or an
// explicit $/cancelRequest), rather than an immediate empty result for the
// window's whole duration — mirroring how gopls itself generally waits for
// its own snapshot to become ready rather than answering empty.
func (s *Server) waitWorkspace(ctx context.Context) *workspace {
	if ws := s.workspace(); ws != nil {
		return ws
	}
	select {
	case <-s.wsReady:
		return s.workspace()
	case <-ctx.Done():
		return nil
	}
}

// markPendingOpen records path as seen by handleDidOpen while s.workspace()
// was still nil, pending SetFocus/Invalidate once one becomes available
// (see drainPendingOpens).
func (s *Server) markPendingOpen(path string) {
	s.pendingOpensMu.Lock()
	defer s.pendingOpensMu.Unlock()
	if s.pendingOpens == nil {
		s.pendingOpens = make(map[string]bool)
	}
	s.pendingOpens[path] = true
}

// takePendingOpens returns every document path markPendingOpen recorded
// since the last takePendingOpens call, clearing the set.
func (s *Server) takePendingOpens() []string {
	s.pendingOpensMu.Lock()
	defer s.pendingOpensMu.Unlock()
	if len(s.pendingOpens) == 0 {
		return nil
	}
	paths := make([]string, 0, len(s.pendingOpens))
	for p := range s.pendingOpens {
		paths = append(paths, p)
	}
	s.pendingOpens = nil
	return paths
}

// drainPendingOpens applies SetFocus/Invalidate — the same bookkeeping
// handleDidOpen itself performs when the workspace is already ready — to
// every document path markPendingOpen recorded while it was not, against
// the just-installed ws. Called unconditionally from setWorkspace: a no-op
// once the pending set has already been drained once (the ordinary case
// for every setWorkspace call after the workspace's first ever install).
func (s *Server) drainPendingOpens(ws *workspace) {
	for _, path := range s.takePendingOpens() {
		if _, ok := ws.nonWorkspacePackageForFile(path); ok {
			continue
		}
		ws.engine.SetFocus(path)
		ws.engine.Invalidate(filepath.Dir(path))
	}
}

// nonWorkspacePackageForFile returns the real import path of the
// graph-known, non-workspace package (GOROOT or a module-cache dependency —
// see internal/depcheck's package doc) containing path, if any. A workspace
// package — one that matched a Load pattern directly (graph.Package.Root) —
// always returns ok=false here, even though fileToPkg/dirToPkg know it too:
// a workspace file keeps using ws.engine's own compilation pipeline
// (internal/typecheck's export-data importer for ITS dependencies remains
// the right tool for compilation input, per internal/depcheck's package
// doc); only a dependency file routes to ws.depProvider (see
// resolveCheckedPackage). Mirrors internal/check.GraphSource.PackageForFile's
// file-then-directory fallback, over the same fileToPkg/dirToPkg maps.
func (ws *workspace) nonWorkspacePackageForFile(path string) (pkgPath string, ok bool) {
	pp, hit := ws.fileToPkg[path]
	if !hit {
		pp, hit = ws.dirToPkg[filepath.Dir(path)]
	}
	if !hit {
		return "", false
	}
	pkg, ok := ws.snap.Package(pp)
	if !ok || pkg.Root {
		return "", false
	}
	return pp, true
}

// pkgPathForFile returns the import path of the package containing path, if
// path is part of the loaded workspace. If path is not itself a known Go
// file (e.g. an in-package _test.go file, which ws.fileToPkg never
// includes — see internal/graph's loadMode), it falls back to matching
// path's directory against a known package's directory, exactly as
// internal/check.GraphSource.PackageForFile does for the same case.
func (s *Server) pkgPathForFile(path string) (string, bool) {
	ws := s.workspace()
	if ws == nil {
		return "", false
	}
	if p, ok := ws.fileToPkg[path]; ok {
		return p, true
	}
	p, ok := ws.dirToPkg[filepath.Dir(path)]
	return p, ok
}
