package server

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"strings"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/xref"
)

// registerCodeActionHandlers registers textDocument/codeAction on s.rpc.
func (s *Server) registerCodeActionHandlers() {
	s.rpc.Handle(protocol.MethodTextDocumentCodeAction, rpc.Interactive, s.handleCodeAction)
}

// handleCodeAction answers textDocument/codeAction: the
// source.organizeImports action (offered whenever it would change the
// file) plus one quickfix per fixable diagnostic in
// params.Context.Diagnostics. Both kinds are filtered against
// params.Context.Only when the client sets it.
func (s *Server) handleCodeAction(_ context.Context, params json.RawMessage) (any, error) {
	var p protocol.CodeActionParams
	if err := protocol.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	path := p.TextDocument.URI.FsPath()
	text, err := s.overlay.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var out []protocol.CodeAction
	if kindRequested(p.Context.Only, protocol.CodeActionKindSourceOrganizeImports) {
		a, ok, err := langfeat.OrganizeImportsAction(path, text)
		if err != nil {
			s.logger.Printf("server: organize imports code action for %s: %v", path, err)
		} else if ok {
			out = append(out, s.toProtocolCodeAction(path, text, nil, a))
		}
	}
	if kindRequested(p.Context.Only, protocol.CodeActionKindQuickFix) {
		out = append(out, s.quickFixActions(path, text, p.Context.Diagnostics)...)
	}
	return out, nil
}

// kindRequested reports whether kind satisfies only: either only is empty
// (the client accepts any kind) or kind equals, or is a dotted refinement
// of, one of only's entries — e.g. only=["source"] accepts
// "source.organizeImports".
func kindRequested(only []protocol.CodeActionKind, kind protocol.CodeActionKind) bool {
	if len(only) == 0 {
		return true
	}
	for _, o := range only {
		if o == kind || strings.HasPrefix(string(kind), string(o)+".") {
			return true
		}
	}
	return false
}

// quickFixActions returns one protocol.CodeAction per fixable diagnostic
// in diags.
func (s *Server) quickFixActions(path string, text []byte, diags []protocol.Diagnostic) []protocol.CodeAction {
	var out []protocol.CodeAction
	for i := range diags {
		d := &diags[i]
		offset, ok := byteOffsetForPosition(text, d.Range.Start)
		if !ok {
			continue
		}
		msg, ok := diagnosticMessage(d)
		if !ok {
			continue
		}
		for _, a := range s.quickFixesForMessage(path, text, offset, msg) {
			out = append(out, s.toProtocolCodeAction(path, text, d, a))
		}
	}
	return out
}

// diagnosticMessage extracts d.Message's plain text, or ok=false if it is
// not the protocol.String variant every golance diagnostic is published as
// (see publishDiagnostics) — a well-behaved client echoes back exactly
// what it received, so this is never false in practice.
func diagnosticMessage(d *protocol.Diagnostic) (string, bool) {
	s, ok := d.Message.(protocol.String)
	return string(s), ok
}

// quickFixesForMessage dispatches one diagnostic message to the langfeat
// fix that handles it, recognized by the fixed substrings the Go compiler
// uses for these diagnostics (see internal/check.Diagnostics, which
// reports go/types errors verbatim).
func (s *Server) quickFixesForMessage(path string, text []byte, offset int, msg string) []langfeat.CodeAction {
	switch {
	case strings.Contains(msg, "imported and not used"):
		if a, ok, err := langfeat.UnusedImportFix(path, text, offset); err == nil && ok {
			return []langfeat.CodeAction{a}
		}
	case strings.Contains(msg, "declared and not used"):
		if a, ok, err := langfeat.UnusedVarFix(path, text, offset); err == nil && ok {
			return []langfeat.CodeAction{a}
		}
	default:
		if name, ok := strings.CutPrefix(msg, "undefined: "); ok {
			return s.addImportFixes(path, text, offset, name)
		}
	}
	return nil
}

// addImportFixes returns one AddImportFix CodeAction per package the
// workspace index knows an exported symbol named name in, or nil if the
// index is not ready yet or knows of none.
func (s *Server) addImportFixes(path string, text []byte, offset int, name string) []langfeat.CodeAction {
	idx := s.idx.Load()
	ws := s.workspace()
	if idx == nil || ws == nil {
		return nil
	}
	pkgPath, ok := ws.fileToPkg[path]
	if !ok {
		return nil
	}
	candidates, err := importCandidates(idx.resolver, ws, pkgPath, name)
	if err != nil || len(candidates) == 0 {
		return nil
	}
	actions, err := langfeat.AddImportFix(path, text, offset, name, candidates)
	if err != nil {
		s.logger.Printf("server: add-import code action for %s: %v", path, err)
		return nil
	}
	return actions
}

// importCandidates returns one langfeat.ImportCandidate per package other
// than pkgPath that exports a package-level symbol named exactly name,
// resolved via resolver's name index.
func importCandidates(resolver *xref.Resolver, ws *workspace, pkgPath, name string) ([]langfeat.ImportCandidate, error) {
	// handleCodeAction ignores its own ctx (see its _ context.Context
	// parameter) since this quickfix-only lookup is a single bounded-size
	// name index scan, not one of the cancellation-sensitive query paths
	// finding 11 targets.
	matches, err := resolver.WorkspaceSymbol(context.Background(), name)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var out []langfeat.ImportCandidate
	for _, m := range matches {
		if m.Name != name || m.Container == pkgPath || seen[m.Container] || !isImportableKind(m.Kind) {
			continue
		}
		pkgName, ok := packageNameOf(ws, m.Container)
		if !ok {
			continue
		}
		seen[m.Container] = true
		out = append(out, langfeat.ImportCandidate{PackageName: pkgName, ImportPath: m.Container})
	}
	return out, nil
}

// isImportableKind reports whether kind is a package-level declaration an
// import can bring into scope — excluding index.KindMethod and
// index.KindField, which are only reachable through a receiver or
// selector and can never be the target of "undefined: Name".
func isImportableKind(kind uint8) bool {
	switch kind {
	case index.KindFunc, index.KindType, index.KindInterface, index.KindVar, index.KindConst:
		return true
	default:
		return false
	}
}

// packageNameOf returns pkgPath's actual declared package name (read from
// its package clause), since that can differ from its import path's last
// component.
func packageNameOf(ws *workspace, pkgPath string) (string, bool) {
	pkg, ok := ws.snap.Packages[pkgPath]
	if !ok || len(pkg.GoFiles) == 0 {
		return "", false
	}
	astFile, err := parser.ParseFile(token.NewFileSet(), pkg.GoFiles[0], nil, parser.PackageClauseOnly)
	if err != nil {
		return "", false
	}
	return astFile.Name.Name, true
}

// toProtocolCodeAction converts a langfeat.CodeAction into a
// protocol.CodeAction for path's document, attaching diag (if non-nil) as
// the diagnostic it resolves.
func (s *Server) toProtocolCodeAction(path string, text []byte, diag *protocol.Diagnostic, a langfeat.CodeAction) protocol.CodeAction {
	edits := make([]protocol.TextEdit, 0, len(a.Edits))
	for _, e := range a.Edits {
		rng, ok := offsetRangeToLSP(text, e.Range.StartOffset, e.Range.EndOffset)
		if !ok {
			continue
		}
		edits = append(edits, protocol.TextEdit{Range: rng, NewText: e.NewText})
	}
	kind := actionKindToProtocol(a.Kind)
	out := protocol.CodeAction{
		Title: a.Title,
		Kind:  &kind,
		Edit:  &protocol.WorkspaceEdit{Changes: map[uri.URI][]protocol.TextEdit{uri.File(path): edits}},
	}
	if diag != nil {
		out.Diagnostics = []protocol.Diagnostic{*diag}
	}
	return out
}

func actionKindToProtocol(k langfeat.ActionKind) protocol.CodeActionKind {
	if k == langfeat.ActionSourceOrganizeImports {
		return protocol.CodeActionKindSourceOrganizeImports
	}
	return protocol.CodeActionKindQuickFix
}
