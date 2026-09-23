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
// tell a file is clean instead of hearing nothing. The one exception is a
// file gated this round (see below): it gets no notification at all, since
// this recheck never actually determined whether it is clean. A file that
// is not open, or that this unit is not authoritative for, is never
// notified: the latter matters now that two units can share res.Dir, so one
// publishing must not clear or otherwise speak for a file only the other
// one checked.
func (s *Server) publishDiagnostics(res *check.Result) {
	byFile, gated := groupDiagsByFile(res.Diags, s.idx.Load() != nil)

	owned := make(map[string]bool, len(res.Files))
	for _, f := range res.Files {
		owned[f] = true
	}

	empty := s.updateDiagFiles(res.PkgPath, res.Dir, owned, byFile, gated)

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

// groupDiagsByFile buckets diags by file into protocol.Diagnostic values,
// dropping (and recording in gated) any diagnostic caused by
// depCacheHolder.importer's cold-build gate while indexReady is false — see
// coldGateSource's own doc: a miss there is routine and temporary while the
// facts index has not finished its first build, not a genuine problem with
// the file being checked, and recheckOpenFilesAfterIndexReady
// (internal/server/indexer.go) gives every such file a fresh, accurate
// recheck the moment the index IS ready. gated matters beyond simple
// filtering because an import go/types could not resolve is never also
// reported as unused — it never gets far enough to determine that — so
// updateDiagFiles must not treat a file whose only diagnostic was gated as
// genuinely clean.
func groupDiagsByFile(diags []check.Diag, indexReady bool) (byFile map[string][]protocol.Diagnostic, gated map[string]bool) {
	byFile = make(map[string][]protocol.Diagnostic)
	gated = make(map[string]bool)
	for _, d := range diags {
		if !indexReady && isColdGateImportDiag(d.Message) {
			gated[d.File] = true
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
	return byFile, gated
}

// updateDiagFiles reconciles s.diagFiles[pkgPath] (the set of files pkgPath
// last published diagnostics for) against byFile and gated, and returns
// every owned file that must now be published an empty diagnostics list:
// one that had diagnostics before but has none now, or that is open and
// owned but has never been published anything. A gated file is excluded
// from both: this round is not authoritative for it (see groupDiagsByFile's
// doc), so whatever s.diagFiles already says about it is carried forward
// unchanged rather than being overwritten with a false "clean" answer.
func (s *Server) updateDiagFiles(pkgPath, dir string, owned map[string]bool, byFile map[string][]protocol.Diagnostic, gated map[string]bool) []string {
	s.diagMu.Lock()
	defer s.diagMu.Unlock()

	prev := s.diagFiles[pkgPath]
	next := make(map[string]bool, len(byFile))
	for file := range byFile {
		next[file] = true
	}
	for file := range gated {
		if prev[file] {
			next[file] = true
		}
	}

	var empty []string
	for file := range prev {
		if !next[file] && !gated[file] {
			empty = append(empty, file)
		}
	}
	for _, file := range s.overlay.OpenFilesInDir(dir) {
		if !owned[file] || next[file] || prev[file] || gated[file] {
			continue
		}
		empty = append(empty, file)
	}

	s.diagFiles[pkgPath] = next
	return empty
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
