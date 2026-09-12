package golance_test

// This file closes audit-informational.md's "UNVERIFIABLE this pass"
// finding for signatureHelp, foldingRange, codeLens, and
// publishDiagnostics: each is driven, over real LSP stdio, against both a
// real golance and a real gopls v0.23.0 (startGoplsClient,
// e2e_gopls_parity_test.go), against the shared
// testdata/module/parityaudit fixture (generics, value/pointer receiver
// methods, variadics, multi-return/named results, embedded types,
// interfaces, a const block, struct literals) and the
// testdata/module/codelens/codelenscgo fixtures already used by golance's
// own non-gopls-compared code lens tests (e2e_codelens_test.go). See also
// internal/langfeat/folding_gopls_parity_test.go, which drives
// `gopls folding_ranges` at the langfeat layer (no live LSP session
// needed) for a finer-grained comparison of langfeat.FoldingRanges alone,
// separate from this file's end-to-end check of the full
// textDocument/foldingRange handler (handlers_nav.go's
// handleFoldingRange). rangeFormatting has no meaningfully comparable
// gopls CLI or LSP shape beyond "reuses go/format" — see
// internal/langfeat/format_test.go and internal/server/
// rangeformat_gopls_parity_test.go for the invariants asserted instead,
// and the one mismatch found and pinned there.

import (
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// requestSignatureHelp sends textDocument/signatureHelp to c and decodes
// the result, or returns nil for a null/empty response.
func requestSignatureHelp(t *testing.T, c *lspClient, path string, pos protocol.Position) *protocol.SignatureHelp {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentSignatureHelp, &protocol.SignatureHelpParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(path)},
			Position:     pos,
		},
	}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("signatureHelp failed: %s", resp.Error)
	}
	if len(resp.Result) == 0 || string(resp.Result) == "null" {
		return nil
	}
	var sh protocol.SignatureHelp
	if err := protocol.Unmarshal(resp.Result, &sh); err != nil {
		t.Fatalf("unmarshal signatureHelp result: %v (raw=%s)", err, resp.Result)
	}
	return &sh
}

// activeParamOf returns sh's active parameter index: SignatureInformation
// .ActiveParameter if the response sets it (the field the LSP spec says
// should be used since 3.16.0), otherwise the top-level SignatureHelp
// .ActiveParameter, otherwise 0 (the spec's own default for both). -1 if
// sh has no signatures at all.
func activeParamOf(sh *protocol.SignatureHelp) int {
	if sh == nil || len(sh.Signatures) == 0 {
		return -1
	}
	if v, ok := sh.Signatures[0].ActiveParameter.Get(); ok {
		return int(v)
	}
	if v, ok := sh.ActiveParameter.Get(); ok {
		return int(v)
	}
	return 0
}

// signatureLabelOf returns sh's first signature's Label, or "" if sh has
// none.
func signatureLabelOf(sh *protocol.SignatureHelp) string {
	if sh == nil || len(sh.Signatures) == 0 {
		return ""
	}
	return sh.Signatures[0].Label
}

// TestE2E_GoplsParity_SignatureHelp drives real golance and real gopls
// over stdio against the identical fixture file and cursor position for
// two shapes signatureHelp has to get right: a multi-return/named-result
// function call (Divide) and a pointer-receiver method call (Translate),
// at both the first and second argument position, checking that golance's
// active parameter index and signature label agree with gopls's.
func TestE2E_GoplsParity_SignatureHelp(t *testing.T) {
	skipUnlessE2E(t)

	root := auditfeatModuleRoot(t)
	file := filepath.Join(root, "parityaudit", "parityaudit.go")
	src := readFixture(t, file)

	gl := startClient(t, root)
	gl.initialize(t, root)
	gl.openFile(t, file)

	gp := startGoplsClient(t, root)
	gp.initialize(t, root)
	gp.openFile(t, file)

	cases := []struct {
		name            string
		marker          string
		wantActiveParam int
	}{
		{name: "Divide/FirstArg", marker: ":= Divide(", wantActiveParam: 0},
		{name: "Divide/SecondArg", marker: "Divide(10, ", wantActiveParam: 1},
		{name: "Translate/FirstArg", marker: "p.Translate(", wantActiveParam: 0},
		{name: "Translate/SecondArg", marker: "p.Translate(3, ", wantActiveParam: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos := posAfter(t, src, tc.marker)

			gpSH := requestSignatureHelp(t, gp, file, pos)
			glSH := requestSignatureHelp(t, gl, file, pos)

			gpLabel := signatureLabelOf(gpSH)
			if gpLabel == "" {
				t.Fatalf("gopls (oracle) returned no signature for %s at %q — fixture/position problem, not a golance bug", tc.name, tc.marker)
			}
			glLabel := signatureLabelOf(glSH)
			if glLabel == "" {
				t.Fatalf("golance signatureHelp is EMPTY for %s while gopls answered %q", tc.name, gpLabel)
			}
			if glLabel != gpLabel {
				t.Errorf("golance signature label = %q, want %q (gopls)", glLabel, gpLabel)
			}

			gpActive := activeParamOf(gpSH)
			if gpActive != tc.wantActiveParam {
				t.Fatalf("gopls (oracle) active parameter = %d, want %d — fixture/position problem", gpActive, tc.wantActiveParam)
			}
			glActive := activeParamOf(glSH)
			if glActive != tc.wantActiveParam {
				t.Errorf("golance active parameter = %d, want %d (gopls agrees on %d)", glActive, tc.wantActiveParam, gpActive)
			}
		})
	}
}

// codeLensTitles returns the Command.Title of every entry in lenses.
func codeLensTitles(lenses []protocol.CodeLens) []string {
	titles := make([]string, len(lenses))
	for i, l := range lenses {
		titles[i] = l.Command.Title
	}
	return titles
}

// containsTitle reports whether titles contains want.
func containsTitle(titles []string, want string) bool {
	for _, ti := range titles {
		if ti == want {
			return true
		}
	}
	return false
}

// TestE2E_GoplsParity_CodeLens drives real golance and real gopls over
// stdio against the shared codelens/codelenscgo fixtures (already used by
// golance's own non-gopls-compared code lens tests, e2e_codelens_test.go),
// with the "test" lens source explicitly enabled on both sides (off by
// default for both, matching gopls's own settings.DefaultOptions()), and
// checks that every lens title gopls reports for a go:generate directive,
// a Test/Benchmark function, and an `import "C"` declaration is also
// present in golance's own answer.
func TestE2E_GoplsParity_CodeLens(t *testing.T) {
	skipUnlessE2E(t)

	root := auditfeatModuleRoot(t)
	generateFile := filepath.Join(root, "codelens", "codelens.go")
	testFile := filepath.Join(root, "codelens", "codelens_test.go")
	cgoFile := filepath.Join(root, "codelenscgo", "codelenscgo.go")

	gl := startClient(t, root)
	initializeWithCodeLenses(t, gl, root, map[string]bool{"test": true})
	gl.openFile(t, generateFile)
	gl.openFile(t, testFile)
	gl.openFile(t, cgoFile)

	gp := startGoplsClient(t, root)
	initializeWithCodeLenses(t, gp, root, map[string]bool{"test": true})
	gp.openFile(t, generateFile)
	gp.openFile(t, testFile)
	gp.openFile(t, cgoFile)

	cases := []struct {
		name   string
		file   string
		titles []string
	}{
		{name: "Generate", file: generateFile, titles: []string{"run go generate ./...", "run go generate"}},
		{name: "TestAndBenchmark", file: testFile, titles: []string{"run test", "run benchmark", "run file benchmarks"}},
		{name: "RegenerateCgo", file: cgoFile, titles: []string{"regenerate cgo definitions"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gpTitles := codeLensTitles(requestCodeLensE2E(t, gp, tc.file))
			glTitles := codeLensTitles(requestCodeLensE2E(t, gl, tc.file))

			for _, want := range tc.titles {
				if !containsTitle(gpTitles, want) {
					t.Fatalf("gopls (oracle) codeLens for %s has no %q entry either — fixture problem: %v", tc.name, want, gpTitles)
				}
				if !containsTitle(glTitles, want) {
					t.Errorf("golance codeLens for %s is missing %q, which gopls offers\ngolance titles: %v\ngopls titles  : %v", tc.name, want, glTitles, gpTitles)
				}
			}
		})
	}
}

// diagnosticAt returns the first entry of diags whose Range.Start.Line
// equals line, or nil if none does.
func diagnosticAt(diags []protocol.Diagnostic, line uint32) *protocol.Diagnostic {
	for i := range diags {
		if diags[i].Range.Start.Line == line {
			return &diags[i]
		}
	}
	return nil
}

// diagnosticMessage extracts msg's text: Diagnostic.Message is typed
// InlayHintTooltip (string | MarkupContent, reused verbatim from the
// inlay hint tooltip shape) even though neither golance nor gopls sends
// the MarkupContent variant here — both always send a plain
// protocol.String, matching hoverText's own reasoning for the analogous
// Hover.Contents union.
func diagnosticMessage(msg protocol.InlayHintTooltip) string {
	switch v := msg.(type) {
	case protocol.String:
		return string(v)
	case *protocol.MarkupContent:
		if v == nil {
			return ""
		}
		return v.Value
	default:
		return ""
	}
}

// TestE2E_GoplsParity_PublishDiagnostics drives real golance and real
// gopls over stdio against a package with a deliberate type error
// (testdata/module/parityauditbroken) and checks golance reports a
// diagnostic at the same start position and with the same message gopls
// does (both surface go/types' own error text verbatim — see
// internal/check/diagnostics.go's Diagnostics), then checks a clean
// package (testdata/module/parityaudit) is reported diagnostic-free by
// both.
func TestE2E_GoplsParity_PublishDiagnostics(t *testing.T) {
	skipUnlessE2E(t)

	root := auditfeatModuleRoot(t)
	brokenFile := filepath.Join(root, "parityauditbroken", "broken.go")
	cleanFile := filepath.Join(root, "parityaudit", "parityaudit.go")

	gl := startClient(t, root)
	gl.initialize(t, root)

	gp := startGoplsClient(t, root)
	gp.initialize(t, root)

	t.Run("TypeError", func(t *testing.T) {
		gl.openFile(t, brokenFile)
		gp.openFile(t, brokenFile)

		gpDiags := gp.waitForDiagnostics(t, brokenFile)
		glDiags := gl.waitForDiagnostics(t, brokenFile)

		// "var n int = "not a number"" is line 8 (0-based) of broken.go.
		const wantLine = 8
		gpDiag := diagnosticAt(gpDiags, wantLine)
		if gpDiag == nil {
			t.Fatalf("gopls (oracle) reported no diagnostic at line %d either — fixture problem: %+v", wantLine, gpDiags)
		}
		glDiag := diagnosticAt(glDiags, wantLine)
		if glDiag == nil {
			t.Fatalf("golance reported no diagnostic at line %d, while gopls did: %+v", wantLine, gpDiag)
		}
		if glDiag.Range.Start != gpDiag.Range.Start {
			t.Errorf("golance diagnostic start = %+v, want %+v (gopls)", glDiag.Range.Start, gpDiag.Range.Start)
		}
		glMsg, gpMsg := diagnosticMessage(glDiag.Message), diagnosticMessage(gpDiag.Message)
		if glMsg != gpMsg {
			t.Errorf("golance diagnostic message = %q, want %q (gopls)", glMsg, gpMsg)
		}
		if glDiag.Severity != protocol.DiagnosticSeverityError {
			t.Errorf("golance diagnostic severity = %v, want DiagnosticSeverityError", glDiag.Severity)
		}
	})

	t.Run("CleanPackage", func(t *testing.T) {
		gl.openFile(t, cleanFile)
		gp.openFile(t, cleanFile)

		gpDiags := gp.waitForDiagnostics(t, cleanFile)
		if len(gpDiags) != 0 {
			t.Fatalf("gopls (oracle) reported diagnostics for a clean package either — fixture problem: %+v", gpDiags)
		}
		glDiags := gl.waitForDiagnostics(t, cleanFile)
		if len(glDiags) != 0 {
			t.Errorf("golance reported diagnostics for a clean package: %+v", glDiags)
		}
	})
}

// requestFoldingRange sends textDocument/foldingRange to c and decodes the
// result.
func requestFoldingRange(t *testing.T, c *lspClient, path string) []protocol.FoldingRange {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentFoldingRange, &protocol.FoldingRangeParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(path)},
	}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("foldingRange failed: %s", resp.Error)
	}
	var ranges []protocol.FoldingRange
	if err := protocol.Unmarshal(resp.Result, &ranges); err != nil {
		t.Fatalf("unmarshal foldingRange result: %v (raw=%s)", err, resp.Result)
	}
	return ranges
}

// lineSpan is a FoldingRange reduced to its (StartLine, EndLine) pair —
// golance never sets StartCharacter/EndCharacter (see handlers_nav.go's
// handleFoldingRange), so only line-granularity parity is meaningful here,
// matching internal/langfeat/folding_gopls_parity_test.go's own reasoning.
type lineSpan struct{ start, end uint32 }

func lineSpansOf(ranges []protocol.FoldingRange) map[lineSpan]bool {
	spans := make(map[lineSpan]bool, len(ranges))
	for _, r := range ranges {
		spans[lineSpan{start: r.StartLine, end: r.EndLine}] = true
	}
	return spans
}

// TestE2E_GoplsParity_FoldingRange drives real golance and real gopls over
// stdio against the shared parityaudit fixture and checks that every
// (StartLine, EndLine) span golance's full textDocument/foldingRange
// handler (handlers_nav.go's handleFoldingRange, not owned by this
// change) reports also appears in gopls's own answer — a containment, not
// an equality, check: golance folds a narrower set of syntax shapes than
// gopls by design (see internal/langfeat/folding_gopls_parity_test.go's
// own doc), so this end-to-end request only needs to confirm the full LSP
// round trip (offset-to-position conversion, kind mapping) introduces no
// wrong or invented span beyond what the langfeat-layer test already
// covers for FoldingRanges itself.
func TestE2E_GoplsParity_FoldingRange(t *testing.T) {
	skipUnlessE2E(t)

	root := auditfeatModuleRoot(t)
	file := filepath.Join(root, "parityaudit", "parityaudit.go")

	gl := startClient(t, root)
	gl.initialize(t, root)
	gl.openFile(t, file)

	gp := startGoplsClient(t, root)
	gp.initialize(t, root)
	gp.openFile(t, file)

	gpSpans := lineSpansOf(requestFoldingRange(t, gp, file))
	glRanges := requestFoldingRange(t, gl, file)
	if len(glRanges) == 0 {
		t.Fatalf("golance foldingRange returned no ranges for a fixture with import/struct/comment/nested-block folds")
	}
	for _, r := range glRanges {
		span := lineSpan{start: r.StartLine, end: r.EndLine}
		if !gpSpans[span] {
			t.Errorf("golance foldingRange reports %+v at line span %d-%d, which gopls does not fold at all", r, r.StartLine, r.EndLine)
		}
	}
}
