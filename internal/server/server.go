// Package server wires golance's independent internal packages (rpc,
// overlay, graph, check, langfeat, xref, index, store) into a running LSP
// server: handler registration, coordinate conversion between the LSP wire
// protocol and each package's own coordinate system, and process lifecycle
// (workspace load, indexer subprocess, incremental reindex).
package server

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"go.lsp.dev/protocol"

	golance "github.com/sivchari/golance"
	"github.com/sivchari/golance/internal/check"
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
	root      string
	snap      *graph.Snapshot
	engine    *check.Engine
	depCache  *depCacheHolder
	fileToPkg map[string]string
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

	diagMu    sync.Mutex
	diagFiles map[string]map[string]bool // package dir -> files last published with diagnostics

	indexBuildingWarned atomic.Bool // one-time window/logMessage while the index is still building
	indexFailedWarned   atomic.Bool // one-time window/showMessage after an indexer failure

	watch           *watchDebouncer // coalesces workspace/didChangeWatchedFiles .go events into revalidateWorkspace passes
	watchDynamicReg atomic.Bool     // client declared workspace.didChangeWatchedFiles.dynamicRegistration support at initialize (see handleInitialized)

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
	}
	s.watch = newWatchDebouncer(opts.WatchDebounce, s.revalidateWorkspace)
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

	s.rpc.Handle(protocol.MethodTextDocumentHover, rpc.Interactive, s.handleHover)
	s.rpc.Handle(protocol.MethodTextDocumentSignatureHelp, rpc.Interactive, s.handleSignatureHelp)
	s.rpc.Handle(protocol.MethodTextDocumentDocumentSymbol, rpc.Interactive, s.handleDocumentSymbol)
	s.rpc.Handle(protocol.MethodTextDocumentInlayHint, rpc.Interactive, s.handleInlayHint)
	s.rpc.Handle(protocol.MethodTextDocumentFormatting, rpc.Interactive, s.handleFormatting)
	s.registerCodeActionHandlers()
	s.registerSemanticHandlers()

	s.rpc.Handle(protocol.MethodTextDocumentDefinition, rpc.Background, s.handleDefinition)
	s.rpc.Handle(protocol.MethodTextDocumentReferences, rpc.Background, s.handleReferences)
	s.rpc.Handle(protocol.MethodTextDocumentImplementation, rpc.Background, s.handleImplementation)
	s.rpc.Handle(protocol.MethodWorkspaceSymbol, rpc.Background, s.handleWorkspaceSymbol)
	s.rpc.Handle(protocol.MethodTextDocumentRename, rpc.Background, s.handleRename)
	s.registerNavHandlers()
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

// pkgPathForFile returns the import path of the package containing path, if
// path is part of the loaded workspace.
func (s *Server) pkgPathForFile(path string) (string, bool) {
	ws := s.workspace()
	if ws == nil {
		return "", false
	}
	p, ok := ws.fileToPkg[path]
	return p, ok
}
