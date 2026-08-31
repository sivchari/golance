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

// handleInitialize resolves the workspace root from params, loads the
// import graph (from cache if warm, else synchronously), and returns
// golance's server capabilities. The facts index is opened directly
// whenever a database already exists (tryWarmOpen), with a cheap
// in-process check (revalidateIndex) running in the background to catch
// it up if anything changed since it was last built; otherwise it is
// built from scratch by launching the indexer subprocess in the
// background (buildIndex).
func (s *Server) handleInitialize(_ context.Context, params json.RawMessage) (any, error) {
	var p protocol.InitializeParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("server: initialize: unmarshal params: %w", err)
	}
	s.setHintsEnabled(parseHintsSettings(p.InitializationOptions))
	s.watchDynamicReg.Store(clientSupportsWatchedFilesRegistration(&p))
	root, err := rootFromInitializeParams(&p, params)
	if err != nil {
		return nil, err
	}

	patterns := []string{"./..."}
	loadOpts := graph.Options{Dir: root, Offline: s.opts.Offline}

	snap, ok := graph.LoadCache(root, patterns, loadOpts.BuildFlags)
	if !ok {
		snap, err = graph.Load(loadOpts, patterns...)
		if err != nil {
			return nil, fmt.Errorf("server: initialize: load import graph: %w", err)
		}
		if err := graph.SaveCache(root, patterns, loadOpts.BuildFlags, snap); err != nil {
			s.logger.Printf("server: save graph cache: %v", err)
		}
	} else if graph.Stale(root) {
		go s.revalidateGraph(loadOpts, patterns)
	}
	s.setWorkspace(root, snap)

	if idx, ok := s.tryWarmOpen(root); ok {
		s.idx.Store(idx)
		go s.revalidateIndex(root)
	} else {
		go s.buildIndex(root)
	}

	return &protocol.InitializeResult{
		Capabilities: s.capabilities(),
		ServerInfo:   protocol.ServerInfo{Name: "golance", Version: protocol.NewOptional(Version)},
	}, nil
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

// handleInitialized responds to the "initialized" notification — the LSP
// spec requires servers to wait for it before sending any server-initiated
// request, including client/registerCapability — by asking the client (in
// the background) to register interest in .go file changes, if it declared
// support for that at "initialize" (see clientSupportsWatchedFilesRegistration).
func (s *Server) handleInitialized(ctx context.Context, _ json.RawMessage) error {
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
