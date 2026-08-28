package server

import (
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/check"
)

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
}

func (s *Server) notifyDiagnostics(file string, diags []protocol.Diagnostic) {
	err := s.rpc.Notify(protocol.MethodTextDocumentPublishDiagnostics, &protocol.PublishDiagnosticsParams{
		URI:         uri.File(file),
		Diagnostics: diags,
	})
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
