package server

import (
	"context"
	"encoding/json"
	"fmt"

	"go.lsp.dev/protocol"

	"github.com/sivchari/golance/internal/graph"
)

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
	root, err := rootFromInitializeParams(&p)
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

// rootFromInitializeParams resolves the workspace root directory from an
// initialize request: the first workspace folder if the client sent any,
// otherwise the deprecated rootUri.
func rootFromInitializeParams(p *protocol.InitializeParams) (string, error) {
	if folders, ok := p.WorkspaceFolders.Get(); ok && len(folders) > 0 {
		return folders[0].URI.FsPath(), nil
	}
	if p.RootURI != nil {
		return p.RootURI.FsPath(), nil
	}
	return "", fmt.Errorf("server: initialize: no rootUri or workspaceFolders in params")
}
