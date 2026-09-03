package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/graph"
)

// registerWatchedFilesTimeout bounds how long registerWatchedFiles waits for
// the client's client/registerCapability response, so a client that never
// answers (or advertised dynamicRegistration without actually honoring the
// request) cannot leak the goroutine handleInitialized starts for it.
const registerWatchedFilesTimeout = 5 * time.Second

// watchedFilesRegistrationID is the Registration.ID golance uses for its one
// workspace/didChangeWatchedFiles registration (see registerWatchedFiles).
// It is never unregistered, so this only needs to be unique among whatever
// else the client itself might register — a fixed string is fine.
const watchedFilesRegistrationID = "golance-watch-go-files"

// allPackagesPattern is the go/packages load pattern golance uses to load
// (and reload) the whole workspace's import graph.
const allPackagesPattern = "./..."

// graphLoad indirects graph.Load, called only from loadWorkspaceAsync, so a
// test can substitute a blocking (or failing) fake without needing a
// synthetic, monorepo-sized module to reproduce a slow cold-worktree load —
// see TestHandleInitialize_ReturnsBeforeGraphLoadCompletes.
var graphLoad = graph.Load

// handleInitialize resolves the workspace root from params and returns
// golance's server capabilities immediately; the import graph load and
// everything gated on it (setWorkspace, the facts-index warm-open/build/
// revalidate decision) run entirely in the background, via
// loadWorkspaceAsync.
//
// Through v0.5.0 this handler ran that whole sequence synchronously before
// responding. On a brand-new git worktree of a large monorepo that made
// "initialize" itself take as long as the import graph load — routinely
// minutes, since GOCACHE is keyed by absolute path and so starts out 100%
// cold for the new worktree's own workspace packages (see loadMode's own
// doc in internal/graph for the -export compilation cost this also
// removes) — comfortably past many editors' own initialize timeout, which
// tears the connection down with no server at all rather than a slow one.
// Every handler that needs the workspace already degrades gracefully while
// s.workspace() is nil (an empty/no-op result, e.g. handlers_langfeat.go's
// handleDocumentSymbol, or resolverOrWarn's own "index still building"
// signal for cross-reference queries); checkedFile — hover, completion,
// signature help, textDocument/definition's index-unavailable fallback,
// and every other per-position feature — additionally blocks briefly on
// waitWorkspace, bounded by the request's own ctx, so a query arriving
// during this now-longer window still gets a real answer once the load
// finishes rather than silence for its whole duration. handleDidOpen
// arriving before the workspace is ready is queued (see
// markPendingOpen/drainPendingOpens) rather than dropped.
func (s *Server) handleInitialize(_ context.Context, params json.RawMessage) (any, error) {
	var p protocol.InitializeParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("server: initialize: unmarshal params: %w", err)
	}
	s.setHintsEnabled(parseHintsSettings(p.InitializationOptions))
	s.setCodeLensesEnabled(parseCodeLensSettings(p.InitializationOptions))
	s.watchDynamicReg.Store(clientSupportsWatchedFilesRegistration(&p))
	s.inlayHintRefreshSupport.Store(clientSupportsInlayHintRefresh(&p))
	s.semanticTokensRefreshSupport.Store(clientSupportsSemanticTokensRefresh(&p))
	root, err := rootFromInitializeParams(&p, params)
	if err != nil {
		return nil, err
	}

	// Bound to the session's own lifetime via s.rpc.Go — not this request's
	// own ctx, which internal/rpc.Server cancels the moment this handler
	// returns (see dispatchRequest) — and tracked by Serve's wg so shutdown
	// waits (briefly) for it instead of leaving it orphaned.
	s.rpc.Go(func(ctx context.Context) { s.loadWorkspaceAsync(ctx, root) })

	// Opportunistic, never blocking this request: remove any session-private
	// index files (see privateIndexDBFile) a crashed prior session left
	// behind for this root.
	s.rpc.Go(func(context.Context) { s.cleanupOrphanedPrivateIndexes(root) })

	return &protocol.InitializeResult{
		Capabilities: s.capabilities(),
		ServerInfo:   protocol.ServerInfo{Name: "golance", Version: protocol.NewOptional(Version)},
	}, nil
}

// loadWorkspaceAsync runs handleInitialize's own former synchronous body:
// load the import graph (from cache if warm, else via graphLoad), install
// it with setWorkspace, then warm-open the facts index directly if a
// database already exists (tryWarmOpen) — with a cheap in-process check
// (revalidateIndex) catching it up in the background if anything changed
// since it was last built — or build it from scratch by launching the
// indexer subprocess (buildIndex) otherwise.
//
// A shared graph cache (graph.Shared — every worktree of one git repository
// reads and writes the same cache file, see graph.CacheFile) is trusted
// immediately for instant readiness even when it might reflect a different
// worktree's own file set (a different branch/commit checked out), rather
// than paying for a `go list` before this worktree's own workspace is ever
// usable at all: graph.Stale's own mtime heuristic is not a reliable
// enough staleness signal for a file shared across worktrees this way (see
// its doc), so a shared cache always additionally kicks a background
// revalidateGraph pass to self-heal, on top of (not instead of) the
// existing Stale check a private, non-shared cache still relies on alone.
func (s *Server) loadWorkspaceAsync(ctx context.Context, root string) {
	patterns := []string{allPackagesPattern}
	loadOpts := graph.Options{Dir: root, Offline: s.opts.Offline}

	snap, ok := graph.LoadCache(root, patterns, loadOpts.BuildFlags)
	if !ok {
		var err error
		snap, err = graphLoad(loadOpts, patterns...)
		if err != nil {
			s.logger.Printf("server: initialize: load import graph: %v", err)
			return
		}
		if err := graph.SaveCache(root, patterns, loadOpts.BuildFlags, snap); err != nil {
			s.logger.Printf("server: save graph cache: %v", err)
		}
	} else if graph.Shared(root) || graph.Stale(root) {
		go s.revalidateGraph(loadOpts, patterns)
	}
	s.setWorkspace(root, snap)

	if idx, ok := s.tryWarmOpen(root); ok {
		s.idx.Store(idx)
		s.revalidateIndex(ctx, root)
	} else {
		s.buildIndex(ctx, root)
	}

	// Opportunistic, low-priority CAS GC: see runStartupCASGC's doc. Backgrounded
	// on its own, separate from loadWorkspaceAsync's own goroutine, so an
	// unlucky worst case (many other index databases in the cache directory,
	// each paying otherDBOpenTimeout) cannot delay anything after this point.
	s.rpc.Go(func(context.Context) { s.runStartupCASGC(root) })
}

// handleShutdown acknowledges the "shutdown" request. internal/rpc already
// transitions the server's lifecycle state; there is nothing else to do.
func (s *Server) handleShutdown(context.Context, json.RawMessage) (any, error) {
	return nil, nil
}

// clientSupportsWatchedFilesRegistration reports whether p's client
// capabilities declare workspace.didChangeWatchedFiles.dynamicRegistration:
// the LSP spec gives servers no other way to receive
// workspace/didChangeWatchedFiles notifications (there is no static
// ServerCapabilities field for it — see
// protocol.DidChangeWatchedFilesClientCapabilities's own doc), so without
// this, handleDidChangeWatchedFiles's .go tracking (see workspace.go) never
// fires for a client that requires registration.
func clientSupportsWatchedFilesRegistration(p *protocol.InitializeParams) bool {
	if p.Capabilities.Workspace == nil || p.Capabilities.Workspace.DidChangeWatchedFiles == nil {
		return false
	}
	dr := p.Capabilities.Workspace.DidChangeWatchedFiles.DynamicRegistration
	return dr != nil && *dr
}

// clientSupportsInlayHintRefresh reports whether p's client capabilities
// declare workspace.inlayHint.refreshSupport: without it, a
// workspace/inlayHint/refresh request (see refreshInlayHints) would be
// sending the client a request it never asked for and may not handle.
func clientSupportsInlayHintRefresh(p *protocol.InitializeParams) bool {
	if p.Capabilities.Workspace == nil || p.Capabilities.Workspace.InlayHint == nil {
		return false
	}
	rs := p.Capabilities.Workspace.InlayHint.RefreshSupport
	return rs != nil && *rs
}

// clientSupportsSemanticTokensRefresh reports whether p's client
// capabilities declare workspace.semanticTokens.refreshSupport: without it,
// a workspace/semanticTokens/refresh request (see refreshSemanticTokens)
// would be sending the client a request it never asked for and may not
// handle.
func clientSupportsSemanticTokensRefresh(p *protocol.InitializeParams) bool {
	if p.Capabilities.Workspace == nil || p.Capabilities.Workspace.SemanticTokens == nil {
		return false
	}
	rs := p.Capabilities.Workspace.SemanticTokens.RefreshSupport
	return rs != nil && *rs
}

// handleInitialized responds to the "initialized" notification — the LSP
// spec requires servers to wait for it before sending any server-initiated
// request, including client/registerCapability — by recording that the
// wait is over (s.clientInitialized; see its doc and
// workspaceReadyRefreshes) and asking the client (in the background) to
// register interest in .go file changes, if it declared support for that
// at "initialize" (see clientSupportsWatchedFilesRegistration).
func (s *Server) handleInitialized(ctx context.Context, _ json.RawMessage) error {
	s.clientInitialized.Store(true)
	if !s.watchDynamicReg.Load() {
		return nil
	}
	go s.registerWatchedFiles(ctx)
	return nil
}

// registerWatchedFiles asks the client, via client/registerCapability, to
// send workspace/didChangeWatchedFiles notifications for every .go file
// (created, changed, or deleted) — the mechanism handleDidChangeWatchedFiles
// relies on to keep the facts index current after a git pull/branch switch
// made outside the editor (see workspace.go and watch.go). ctx is bounded by
// registerWatchedFilesTimeout so a client that never responds cannot leak
// this goroutine.
func (s *Server) registerWatchedFiles(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, registerWatchedFilesTimeout)
	defer cancel()

	opts := protocol.DidChangeWatchedFilesRegistrationOptions{
		Watchers: []protocol.FileSystemWatcher{
			{
				GlobPattern: protocol.Pattern("**/*.go"),
				Kind:        protocol.WatchKindCreate | protocol.WatchKindChange | protocol.WatchKindDelete,
			},
		},
	}
	optsJSON, err := protocol.Marshal(opts)
	if err != nil {
		s.logger.Printf("server: marshal watched-files registration options: %v", err)
		return
	}
	params := &protocol.RegistrationParams{
		Registrations: []protocol.Registration{{
			ID:              watchedFilesRegistrationID,
			Method:          protocol.MethodWorkspaceDidChangeWatchedFiles,
			RegisterOptions: protocol.LSPAny(optsJSON),
		}},
	}
	if _, err := s.rpc.Request(ctx, protocol.MethodClientRegisterCapability, params); err != nil {
		s.logger.Printf("server: register workspace/didChangeWatchedFiles: %v", err)
	}
}

// rootFromInitializeParams resolves the workspace root directory from an
// initialize request: the first workspace folder if the client sent any,
// otherwise the deprecated rootUri, read directly from the raw params
// rather than through protocol.InitializeParams.RootURI (deprecated in
// favour of workspaceFolders) — the LSP spec still requires servers to
// fall back to it for clients that predate workspaceFolders, so it cannot
// simply be dropped.
func rootFromInitializeParams(p *protocol.InitializeParams, params json.RawMessage) (string, error) {
	if folders, ok := p.WorkspaceFolders.Get(); ok && len(folders) > 0 {
		return folders[0].URI.FsPath(), nil
	}
	var legacy struct {
		RootURI *uri.URI `json:"rootUri"`
	}
	if err := json.Unmarshal(params, &legacy); err == nil && legacy.RootURI != nil {
		return legacy.RootURI.FsPath(), nil
	}
	return "", fmt.Errorf("server: initialize: no rootUri or workspaceFolders in params")
}
