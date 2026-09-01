package server

import (
	"context"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/check"
)

// refreshInlayHintsTimeout bounds how long refreshInlayHints waits for the
// client's workspace/inlayHint/refresh response, so a client that never
// answers cannot leak the goroutine publishDiagnostics starts for it.
const refreshInlayHintsTimeout = 5 * time.Second

// refreshSemanticTokensTimeout bounds how long refreshSemanticTokens waits
// for the client's workspace/semanticTokens/refresh response, matching
// refreshInlayHintsTimeout's reasoning.
const refreshSemanticTokensTimeout = 5 * time.Second

// publishDiagnostics is registered as check.Options.OnResult: it converts a
// recheck's diagnostics into textDocument/publishDiagnostics notifications,
// one per file. Every open file in res.Dir gets a notification: files with
// diagnostics get them, and every other open file gets an empty list —
// whether it previously had diagnostics that are now gone, or it has never
// had any diagnostics at all — so the client can tell a file is clean
// instead of hearing nothing. A file that is not open is never notified,
// to avoid spamming clients about files they are not displaying.
func (s *Server) publishDiagnostics(res *check.Result) {
	byFile := make(map[string][]protocol.Diagnostic)
	for _, d := range res.Diags {
		byFile[d.File] = append(byFile[d.File], protocol.Diagnostic{
			Range: protocol.Range{
				Start: protocol.Position{Line: d.StartLine, Character: d.StartCol},
				End:   protocol.Position{Line: d.EndLine, Character: d.EndCol},
			},
			Severity: diagnosticSeverity(d.Severity),
			Source:   protocol.NewOptional("golance"),
			Message:  protocol.String(d.Message),
		})
	}

	s.diagMu.Lock()
	prev := s.diagFiles[res.Dir]
	next := make(map[string]bool, len(byFile))
	for file := range byFile {
		next[file] = true
	}
	var empty []string
	for file := range prev {
		if !next[file] {
			empty = append(empty, file)
		}
	}
	for _, file := range s.overlay.OpenFilesInDir(res.Dir) {
		if next[file] || prev[file] {
			continue
		}
		empty = append(empty, file)
	}
	s.diagFiles[res.Dir] = next
	s.diagMu.Unlock()

	for _, file := range empty {
		s.notifyDiagnostics(file, nil)
	}
	for file, diags := range byFile {
		s.notifyDiagnostics(file, diags)
	}

	// Tell a client that declared workspace.inlayHint.refreshSupport its
	// currently shown inlay hints may now be stale — e.g. res reflects an
	// edit to a dependency, not the open file's own didChange, so the
	// client's usual re-request-on-edit behavior never fires for it. Run
	// via s.rpc.Go (detached, not awaited here): OnResult callers document
	// that publishDiagnostics must not block for long, and Request itself
	// blocks until the client responds.
	if s.inlayHintRefreshSupport.Load() {
		s.rpc.Go(s.refreshInlayHints)
	}
}

// refreshInlayHints sends workspace/inlayHint/refresh, asking the client to
// re-request inlay hints for every currently shown document. Per the LSP
// spec this refresh is global (the request carries no params), so it is
// sent regardless of which directory's recheck triggered it.
func (s *Server) refreshInlayHints(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, refreshInlayHintsTimeout)
	defer cancel()
	if _, err := s.rpc.Request(ctx, protocol.MethodWorkspaceInlayHintRefresh, nil); err != nil {
		s.logger.Printf("server: refresh inlay hints: %v", err)
	}
}

// refreshSemanticTokens sends workspace/semanticTokens/refresh, asking the
// client to re-request semantic tokens for every currently shown document.
// Per the LSP spec this refresh is global (the request carries no params),
// matching refreshInlayHints.
func (s *Server) refreshSemanticTokens(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, refreshSemanticTokensTimeout)
	defer cancel()
	if _, err := s.rpc.Request(ctx, protocol.MethodWorkspaceSemanticTokensRefresh, nil); err != nil {
		s.logger.Printf("server: refresh semantic tokens: %v", err)
	}
}

func (s *Server) notifyDiagnostics(file string, diags []protocol.Diagnostic) {
	params := &protocol.PublishDiagnosticsParams{
		URI:         uri.File(file),
		Diagnostics: diags,
	}
	// Set Version whenever the file is still open, so the client can
	// discard/reconcile this publish against whatever version it currently
	// has instead of trusting an out-of-order notification blindly. A file
	// this reports on is always open (see publishDiagnostics's doc), so
	// !ok here only means it was closed in the narrow window between that
	// check and this notification.
	if _, version, _, ok := s.overlay.Get(uri.File(file)); ok {
		params.Version = protocol.NewOptional(version)
	}
	err := s.rpc.Notify(protocol.MethodTextDocumentPublishDiagnostics, params)
	if err != nil {
		s.logger.Printf("server: publish diagnostics for %s: %v", file, err)
	}
}

func diagnosticSeverity(sev check.Severity) protocol.DiagnosticSeverity {
	if sev == check.SeverityWarning {
		return protocol.DiagnosticSeverityWarning
	}
	return protocol.DiagnosticSeverityError
}
