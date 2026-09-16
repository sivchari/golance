package server

import (
	"context"
	"strings"
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
// one per file. Every open file res is authoritative for — res.Files, the
// files that unit was actually checked against, not just res.Dir's open
// files (a directory can hold two independent units, its base package and
// its external "_test" package, each publishing its own Result for the
// same Dir over a disjoint file set — see internal/check's unitKey) — gets
// a notification: files with diagnostics get them, and every other such
// file gets an empty list — whether it previously had diagnostics that are
// now gone, or it has never had any diagnostics at all — so the client can
// tell a file is clean instead of hearing nothing. A file that is not open,
// or that this unit is not authoritative for, is never notified: the
// latter matters now that two units can share res.Dir, so one publishing
// must not clear or otherwise speak for a file only the other one checked.
func (s *Server) publishDiagnostics(res *check.Result) {
	// While the facts index is not yet ready (a cold build still running —
	// see coldGateSource's own doc), depCacheHolder.importer resolves every
	// workspace-dependency import through a gate that answers only from
	// already-persisted data, by design: a miss there is routine and
	// temporary, not a genuine problem with the file being checked. Without
	// this filter, every open file importing another workspace package
	// shows a "could not import" diagnostic for the whole span of the cold
	// build, even though nothing is actually wrong — see
	// recheckOpenFilesAfterIndexReady (internal/server/indexer.go) for how
	// those same files get a fresh, accurate recheck the moment the index
	// IS ready, so suppressing this now never hides a diagnostic
	// permanently, only until there is a real answer for it.
	indexReady := s.idx.Load() != nil
	byFile := make(map[string][]protocol.Diagnostic)
	for _, d := range res.Diags {
		if !indexReady && isColdGateImportDiag(d.Message) {
			continue
		}
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

	owned := make(map[string]bool, len(res.Files))
	for _, f := range res.Files {
		owned[f] = true
	}

	s.diagMu.Lock()
	prev := s.diagFiles[res.PkgPath]
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
		if !owned[file] || next[file] || prev[file] {
			continue
		}
		empty = append(empty, file)
	}
	s.diagFiles[res.PkgPath] = next
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

// isColdGateImportDiag reports whether msg is the "could not import ..."
// diagnostic go/types produces (go/types/resolver.go: "could not import
// %s (%s)") for an import depCacheHolder.importer's cold-build gate (see
// coldGateSource's own doc) could not yet resolve -- as opposed to a
// genuine syntax/type error in the file being checked. Matching is by
// substring on coldGateMissMarker, embedded verbatim in
// coldGateSource.ExportData's own error text: it is the only mechanism
// available here, since go/types folds whatever ImportFrom returns down
// to that one Msg string before this package ever sees it, discarding
// both its Go type and the unexported Code (BrokenImport) go/types itself
// records for the same failure internally but never exposes outside
// go/types' own package (internal/types/errors, which this module cannot
// import at all -- unlike the equally-unexported go116start/go116end
// token.Pos fields typeErrorRange already reads via reflection, whose
// type is plain and exported).
func isColdGateImportDiag(msg string) bool {
	return strings.Contains(msg, coldGateMissMarker)
}
