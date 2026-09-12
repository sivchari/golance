package golance_test

// This file drives a real gopls (golang.org/x/tools/gopls) over stdio as
// the parity oracle for golance's own textDocument/hover and
// textDocument/completion answers, against the shared fixture module at
// internal/langfeat/testdata/module/auditfeat (see that directory's own
// files for what it covers: generics, interface embedding, cross-package
// symbols, struct tags, iota consts, variadics, multi-return/named
// results, channels/select, and a connect.Request[T]-shaped generic
// wrapper). Both hover and completion are driven over the exact same LSP
// wire protocol, position, and fixture files for both servers, so any
// difference in behavior — not just wording — surfaces directly. Skipped
// (not failed) if gopls is not on PATH, mirroring skipUnlessE2E's own
// "degrade to skip, never silently pass" policy for a missing prerequisite.

import (
	"bufio"
	"encoding/json"
	"math"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

var (
	goplsOnce sync.Once
	goplsBin  string
)

// goplsPath returns the absolute path to a gopls binary on PATH, or "" if
// none is found. Memoized process-wide, mirroring buildGolanceBinary's own
// pattern.
func goplsPath() string {
	goplsOnce.Do(func() {
		if p, err := exec.LookPath("gopls"); err == nil {
			goplsBin = p
		}
	})
	return goplsBin
}

// runGopls starts bin with no arguments, mirroring e2e_client_test.go's
// own runGolance: bin is a function parameter, so gosec's "subprocess
// launched with variable" check exempts it as the executable name (its
// own rule carves out parameters/receivers used in that position).
func runGopls(bin string) *exec.Cmd {
	return exec.Command(bin)
}

// startGoplsClient starts a real gopls over stdio for workspace root,
// reusing lspClient (e2e_client_test.go) and the same isolated env
// startClientIn gives golance (fake HOME, the e2e suite's shared GOCACHE,
// real GOPATH/GOMODCACHE) so gopls's own go/packages.Load neither touches
// the developer's real caches nor needs network access. Skips the test if
// gopls is not on PATH.
func startGoplsClient(t *testing.T, root string) *lspClient {
	t.Helper()
	bin := goplsPath()
	if bin == "" {
		t.Skip("gopls not found on PATH; skipping gopls-parity check")
	}

	fakeHome := t.TempDir()
	cmd := runGopls(bin)
	cmd.Dir = root
	cmd.Env = e2eEnv(t, fakeHome)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("gopls stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("gopls stdout pipe: %v", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start gopls: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		if t.Failed() && stderr.Len() > 0 {
			t.Logf("gopls stderr:\n%s", stderr.String())
		}
	})

	c := &lspClient{
		cmd:         cmd,
		in:          newFrameWriter(stdin),
		out:         bufio.NewReaderSize(stdout, 1<<20),
		pending:     map[string]chan *message{},
		diagnostics: make(chan diagnosticsNotification, 128),
		progress:    make(chan progressNotification, 128),
		requests:    make(chan serverRequest, 128),
	}
	go c.readLoop()

	return c
}

// auditfeatModuleRoot returns the absolute path to the shared audit
// fixture's module root (internal/langfeat/testdata/module, where its
// go.mod lives) — the same directory internal/langfeat's own tests load
// via graph.Load.
func auditfeatModuleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("internal", "langfeat", "testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	return root
}

// requestHover sends textDocument/hover to c and decodes the result, or
// returns nil for a null/empty response.
func requestHover(t *testing.T, c *lspClient, path string, pos protocol.Position) *protocol.Hover {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentHover, &protocol.HoverParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(path)},
			Position:     pos,
		},
	}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("hover failed: %s", resp.Error)
	}
	if len(resp.Result) == 0 || string(resp.Result) == "null" {
		return nil
	}
	var h protocol.Hover
	if err := protocol.Unmarshal(resp.Result, &h); err != nil {
		t.Fatalf("unmarshal hover result: %v (raw=%s)", err, resp.Result)
	}
	return &h
}

// hoverText extracts h's rendered content as plain text. go.lsp.dev/
// protocol's HoverContents is one of *MarkupContent, String, or two
// deprecated MarkedString-shaped variants (superseded by MarkupContent);
// neither golance nor gopls sends the deprecated shapes to a client (like
// this minimal one) that never advertises legacy hover capabilities, so
// only the two current variants are handled here.
func hoverText(h *protocol.Hover) string {
	if h == nil {
		return ""
	}
	switch v := h.Contents.(type) {
	case *protocol.MarkupContent:
		if v == nil {
			return ""
		}
		return v.Value
	case protocol.String:
		return string(v)
	default:
		return ""
	}
}

// completionLabel is the one field requestCompletion needs out of an LSP
// CompletionItem.
type completionLabel struct {
	Label string `json:"label"`
}

// requestCompletion sends textDocument/completion to c and returns the
// candidate labels, handling both a bare CompletionItem[] and a
// CompletionList{items: [...]} response shape (the LSP spec allows
// either; gopls returns a CompletionList, golance a bare array — see
// handleCompletion).
func requestCompletion(t *testing.T, c *lspClient, path string, pos protocol.Position) []string {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentCompletion, &protocol.CompletionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(path)},
			Position:     pos,
		},
	}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("completion failed: %s", resp.Error)
	}
	if len(resp.Result) == 0 || string(resp.Result) == "null" {
		return nil
	}

	var items []completionLabel
	if err := json.Unmarshal(resp.Result, &items); err == nil {
		return completionLabelsOf(items)
	}

	var list struct {
		Items []completionLabel `json:"items"`
	}
	if err := json.Unmarshal(resp.Result, &list); err != nil {
		t.Fatalf("unmarshal completion result: %v (raw=%s)", err, resp.Result)
	}
	return completionLabelsOf(list.Items)
}

func completionLabelsOf(items []completionLabel) []string {
	labels := make([]string, len(items))
	for i, it := range items {
		labels[i] = it.Label
	}
	return labels
}

// posAfter returns the position of the first byte right after marker's
// first occurrence in content — an ASCII-only cursor-placement helper
// (unlike mustPos, which anchors at a token's START) for a completion
// request positioned right after a "x." selector.
func posAfter(t *testing.T, content, marker string) protocol.Position {
	t.Helper()
	for i, line := range strings.Split(content, "\n") {
		idx := strings.Index(line, marker)
		if idx < 0 {
			continue
		}
		col := idx + len(marker)
		if i < 0 || i > math.MaxUint32 || col < 0 || col > math.MaxUint32 {
			t.Fatalf("line/column %d/%d exceeds uint32", i, col)
			return protocol.Position{}
		}
		return protocol.Position{Line: uint32(i), Character: uint32(col)}
	}
	t.Fatalf("marker %q not found in:\n%s", marker, content)
	return protocol.Position{}
}

// containsLabel reports whether labels contains want.
func containsLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// TestE2E_GoplsParity_Hover drives real golance and real gopls binaries
// over stdio against the identical fixture file and cursor position, for
// a handful of shapes golance's Hover (internal/langfeat/hover.go) has to
// get right: a generic function's signature+doc, a generic method's
// signature+doc, a type with a promoted embedded method, and a
// cross-package symbol. It is a soft parity check, not a byte-for-byte
// diff (see hoverText's doc — the two servers' markdown formatting
// legitimately differs): both must produce a non-empty hover, and
// golance's own hover must actually contain the doc comment and signature
// substrings gopls also surfaces, catching truncated/dropped content
// rather than cosmetic wording differences.
func TestE2E_GoplsParity_Hover(t *testing.T) {
	skipUnlessE2E(t)

	root := auditfeatModuleRoot(t)
	wrapperFile := filepath.Join(root, "auditfeat", "wrapper.go")
	ifaceFile := filepath.Join(root, "auditfeat", "iface.go")
	crossFile := filepath.Join(root, "auditfeat", "crosspkg.go")
	wrapperSrc := readFixture(t, wrapperFile)
	ifaceSrc := readFixture(t, ifaceFile)
	crossSrc := readFixture(t, crossFile)

	gl := startClient(t, root)
	gl.initialize(t, root)
	gl.openFile(t, wrapperFile)
	gl.openFile(t, ifaceFile)
	gl.openFile(t, crossFile)
	gl.waitForIndexReady(t)

	gp := startGoplsClient(t, root)
	gp.initialize(t, root)
	gp.openFile(t, wrapperFile)
	gp.openFile(t, ifaceFile)
	gp.openFile(t, crossFile)

	cases := []struct {
		name           string
		file           string
		src            string
		lineSubstr     string
		token          string
		wantGolanceAll []string // substrings golance's own hover must contain
	}{
		{
			name:           "GenericFunc",
			file:           wrapperFile,
			src:            wrapperSrc,
			lineSubstr:     "func Sum[T Numeric]",
			token:          "Sum",
			wantGolanceAll: []string{"Sum", "returns the sum of vals"},
		},
		{
			name:           "GenericMethod",
			file:           wrapperFile,
			src:            wrapperSrc,
			lineSubstr:     "func (r *Request[T]) Any",
			token:          "Any",
			wantGolanceAll: []string{"Any", "underlying message"},
		},
		{
			name:           "PromotedEmbeddedMethodDoc",
			file:           ifaceFile,
			src:            ifaceSrc,
			lineSubstr:     "func (b *baseServer) Name",
			token:          "Name",
			wantGolanceAll: []string{"Name", "returns b's name"},
		},
		{
			name:           "CrossPackageFunc",
			file:           crossFile,
			src:            crossSrc,
			lineSubstr:     "auditsub.NewWidget",
			token:          "NewWidget",
			wantGolanceAll: []string{"NewWidget", "returns a Widget with the given id"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos := mustPos(t, tc.src, tc.lineSubstr, tc.token)

			glHover := hoverText(requestHover(t, gl, tc.file, pos))
			gpHover := hoverText(requestHover(t, gp, tc.file, pos))

			if gpHover == "" {
				t.Fatalf("gopls (oracle) returned an empty hover for %s at %s %q — fixture/position problem, not a golance bug", tc.name, tc.lineSubstr, tc.token)
			}
			if glHover == "" {
				t.Fatalf("golance hover is EMPTY for %s while gopls answered %d bytes: %q", tc.name, len(gpHover), gpHover)
			}
			for _, want := range tc.wantGolanceAll {
				if !strings.Contains(glHover, want) {
					t.Errorf("golance hover missing %q for %s\ngolance: %s\ngopls  : %s", want, tc.name, glHover, gpHover)
				}
			}
		})
	}
}

// TestE2E_GoplsParity_InlayHint_GenericCall drives real golance and real
// gopls over stdio against the auditfeat_test.go fixture's
// auditfeat.Sum(1, 2, 3) call site — a generic function invoked with
// inferred (not explicit) type arguments — checking that golance's
// functionTypeParameters inlay hint (funcTypeParamHint in
// internal/langfeat/inlayhints.go) fires there exactly as gopls's own
// does.
func TestE2E_GoplsParity_InlayHint_GenericCall(t *testing.T) {
	skipUnlessE2E(t)

	root := auditfeatModuleRoot(t)
	testFile := filepath.Join(root, "auditfeat", "auditfeat_test.go")
	testSrc := readFixture(t, testFile)
	fullRange := protocol.Range{End: endOfDocument(testSrc)}

	// gopls defaults every inlay hint kind OFF (golance's inlayhints.go
	// documents this exact difference — its own default is every kind ON),
	// so functionTypeParameters must be explicitly requested for the oracle
	// too, via the same "hints" initializationOptions key gopls itself
	// defines (golance's own ResolveHints mirrors that key 1:1).
	gl := startClient(t, root)
	initializeWithHints(t, gl, root, nil)
	gl.openFile(t, testFile)

	gp := startGoplsClient(t, root)
	initializeWithHints(t, gp, root, map[string]bool{"functionTypeParameters": true})
	gp.openFile(t, testFile)

	glHints := requestInlayHints(t, gl, testFile, fullRange)
	gpHints := requestInlayHints(t, gp, testFile, fullRange)

	if !hasInlayHintLabelText(gpHints, "[int]") {
		t.Fatalf("gopls (oracle) has no %q inlay hint for the Sum(1, 2, 3) call either — fixture problem: %+v", "[int]", gpHints)
	}
	if !hasInlayHintLabelText(glHints, "[int]") {
		t.Errorf("golance is missing the %q functionTypeParameters inlay hint gopls gives for the generic Sum(1, 2, 3) call: %+v", "[int]", glHints)
	}
}

// hasInlayHintLabelText is hasInlayHintLabel (e2e_inlayhint_test.go)
// generalized over InlayHint.Label's other wire shape: gopls renders even
// a plain-text hint as []InlayHintLabelPart (each with its own Value),
// while golance always sends the plain protocol.String shape
// hasInlayHintLabel alone checks for — both are legal per the LSP spec.
func hasInlayHintLabelText(hints []protocol.InlayHint, want string) bool {
	for _, h := range hints {
		switch label := h.Label.(type) {
		case protocol.String:
			if string(label) == want {
				return true
			}
		case protocol.InlayHintLabelPartSlice:
			var sb strings.Builder
			for i := range label {
				sb.WriteString(label[i].Value)
			}
			if sb.String() == want {
				return true
			}
		}
	}
	return false
}

// TestE2E_Hover_CrossPackageDoc_RacesIndexReady documents a real
// present-but-incomplete answer TestE2E_GoplsParity_Hover's own
// CrossPackageFunc case works around with an explicit waitForIndexReady:
// hover on a symbol declared in a DIFFERENT workspace package resolves its
// doc comment through the background facts index (see
// handlers_langfeat.go's crossPackageDoc / resolverOrWarn), which is still
// nil for a brief window right after a session starts (buildIndexLocked
// has not yet Store'd a Resolver — see newTestServerNoIndex's own doc in
// internal/server/server_test.go). crossPackageDoc degrades gracefully in
// that window (info.Doc stays "" rather than erroring), but the resulting
// hover is indistinguishable from "this symbol genuinely has no doc
// comment" — nothing in the response signals "index still warming up,
// retry" the way, e.g., indexUnavailableRPCCode does for
// definition/references. A user hovering a cross-package symbol in the
// first moments after opening an editor session sees a doc-less hover
// where gopls (source-based, no async index) never would.
func TestE2E_Hover_CrossPackageDoc_RacesIndexReady(t *testing.T) {
	skipUnlessE2E(t)

	root := auditfeatModuleRoot(t)
	crossFile := filepath.Join(root, "auditfeat", "crosspkg.go")
	crossSrc := readFixture(t, crossFile)
	pos := mustPos(t, crossSrc, "auditsub.NewWidget", "NewWidget")

	gl := startClient(t, root)
	gl.initialize(t, root)
	gl.openFile(t, crossFile)

	const wantDoc = "returns a Widget with the given id"

	gl.waitForIndexReady(t)
	deadline := time.Now().Add(e2eRequestBudget)
	var after string
	var attempts int
	for time.Now().Before(deadline) {
		attempts++
		after = hoverText(requestHover(t, gl, crossFile, pos))
		if strings.Contains(after, wantDoc) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("doc appeared after %d hover attempt(s) following waitForIndexReady", attempts)
	if !strings.Contains(after, wantDoc) {
		t.Errorf("hover doc still missing after waitForIndexReady (retried for %s): %s", e2eRequestBudget, after)
	}
}

// TestE2E_GoplsParity_Completion drives real golance and real gopls over
// stdio against the identical unsaved-buffer fixture and cursor position,
// checking that golance's member-completion for a cross-package generic
// instantiation (auditsub.Box[int]) offers the same field gopls does.
func TestE2E_GoplsParity_Completion(t *testing.T) {
	skipUnlessE2E(t)

	root := auditfeatModuleRoot(t)
	scratchFile := filepath.Join(root, "auditfeat", "zz_completion_scratch.go")
	content := "package auditfeat\n\n" +
		"import \"example.com/langfeatmod/auditfeat/auditsub\"\n\n" +
		"func useBoxCompletion() {\n" +
		"\tb := auditsub.Box[int]{Value: 42}\n" +
		"\t_ = b.\n" +
		"}\n"
	pos := posAfter(t, content, "_ = b.")

	gl := startClient(t, root)
	gl.initialize(t, root)
	gl.openNewFile(t, scratchFile, content)

	gp := startGoplsClient(t, root)
	gp.initialize(t, root)
	gp.openNewFile(t, scratchFile, content)

	glLabels := requestCompletion(t, gl, scratchFile, pos)
	gpLabels := requestCompletion(t, gp, scratchFile, pos)

	if !containsLabel(gpLabels, "Value") {
		t.Fatalf("gopls (oracle) completion for b. does not offer %q either — fixture/position problem: %v", "Value", gpLabels)
	}
	if !containsLabel(glLabels, "Value") {
		t.Errorf("golance completion for b. (auditsub.Box[int]) is missing %q, which gopls offers\ngolance labels: %v\ngopls labels  : %v", "Value", glLabels, gpLabels)
	}
}
