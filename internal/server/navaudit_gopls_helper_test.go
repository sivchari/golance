package server

// This file drives the real gopls v0.23.0 binary -- the oracle for the
// golance-vs-gopls navigation parity audit (see navaudit_parity_test.go and
// ../../audit-navigation.md) -- two ways: a single, long-lived `gopls
// serve` session, driven directly over LSP stdio using the same
// go.lsp.dev/jsonrpc2 and go.lsp.dev/protocol dependencies golance itself
// already uses to speak the protocol (see sharedGopls's doc for why this
// suite shares one session across every case and file instead of spawning
// a fresh `gopls <subcommand>` process per case); and, only where a second,
// independently-driven fixture needs its own throwaway process (external
// test package rename, see externaltest_test.go) or a physical on-disk
// rename to `go build`/`go vet` against, the CLI subcommands this file
// still exposes (definition, references, rename).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// requireGopls skips t unless a gopls binary is on PATH, per the audit's
// mandate to skip (not fail) when the oracle is absent.
func requireGopls(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not found on PATH; skipping golance-vs-gopls parity check")
	}
}

// goplsCacheEnv returns the GOCACHE/GOPLSCACHE overrides every gopls
// invocation (CLI or LSP) needs in this sandboxed environment: gopls's
// default cache locations (GOCACHE, and its own analysis cache under
// os.UserCacheDir()) live outside the sandbox's writable set.
func goplsCacheEnv(cacheDir string) []string {
	return []string{
		"GOCACHE=" + filepath.Join(cacheDir, "gocache"),
		"GOPLSCACHE=" + filepath.Join(cacheDir, "goplscache"),
	}
}

// navLoc is a tool-agnostic, 0-based (line, UTF-16-or-byte character)
// location, normalized from either golance's protocol.Location results or
// gopls's own output, so the two can be compared directly. The fixture
// module (internal/server/testdata/module/navaudit/...) is pure ASCII, so a
// UTF-16 code unit, a byte, and a rune all coincide -- the simple -1/+1
// conversions below are exact for it without needing a real UTF-16 index.
type navLoc struct {
	file            string
	line, col       int
	endLine, endCol int
}

func (l navLoc) String() string {
	if l.endLine == l.line {
		return fmt.Sprintf("%s:%d:%d-%d", filepath.Base(l.file), l.line+1, l.col+1, l.endCol+1)
	}
	return fmt.Sprintf("%s:%d:%d-%d:%d", filepath.Base(l.file), l.line+1, l.col+1, l.endLine+1, l.endCol+1)
}

func locFromLSP(loc protocol.Location) navLoc {
	return navLoc{
		file:    loc.URI.FsPath(),
		line:    int(loc.Range.Start.Line),
		col:     int(loc.Range.Start.Character),
		endLine: int(loc.Range.End.Line),
		endCol:  int(loc.Range.End.Character),
	}
}

func locsFromLSP(locs protocol.LocationSlice) []navLoc {
	out := make([]navLoc, 0, len(locs))
	for _, l := range locs {
		out = append(out, locFromLSP(l))
	}
	return out
}

func locFromGoplsSpan(file string, line, col, endLine, endCol int) navLoc {
	return navLoc{file: file, line: line - 1, col: col - 1, endLine: endLine - 1, endCol: endCol - 1}
}

func sortLocs(locs []navLoc) {
	sort.Slice(locs, func(i, j int) bool {
		a, b := locs[i], locs[j]
		if a.file != b.file {
			return a.file < b.file
		}
		if a.line != b.line {
			return a.line < b.line
		}
		return a.col < b.col
	})
}

// locsEqualSet reports whether a and b contain the same navLocs, order
// insensitive -- the audit's contract for multi-location answers
// (references, implementation, call hierarchy edges).
func locsEqualSet(a, b []navLoc) bool {
	if len(a) != len(b) {
		return false
	}
	sortLocs(a)
	sortLocs(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func locsString(locs []navLoc) string {
	parts := make([]string, len(locs))
	for i, l := range locs {
		parts[i] = l.String()
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// spanLineRE matches one line of gopls's plain-text span output: a file
// path, then a 1-based line:col point, or a col-col (same-line) or
// line:col-line:col (multi-line) range -- the format references still
// emits (see runGopls's doc for why references keeps a CLI path).
var spanLineRE = regexp.MustCompile(`^(.+):(\d+):(\d+)-(?:(\d+):)?(\d+)$`)

// parseSpanLines parses every span-shaped line of output (see spanLineRE),
// silently skipping lines that do not match (e.g. blank lines).
func parseSpanLines(output string) []navLoc {
	var out []navLoc
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		m := spanLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		l, _ := strconv.Atoi(m[2])
		c, _ := strconv.Atoi(m[3])
		endLine := l
		if m[4] != "" {
			endLine, _ = strconv.Atoi(m[4])
		}
		endCol, _ := strconv.Atoi(m[5])
		out = append(out, locFromGoplsSpan(m[1], l, c, endLine, endCol))
	}
	return out
}

// runGopls runs `gopls references <posArg>` (a fresh process per call,
// matching how a shell user would drive it) against root, returning its
// combined output. A non-zero exit is not itself fatal -- "no identifier
// found" and similar routine misses exit non-zero. cacheDir is the
// caller's own t.TempDir(). This suite's own navaudit_parity_test.go no
// longer calls this: it shares one `gopls serve` LSP session instead (see
// sharedGopls's doc) -- externaltest_test.go, driving a second, unrelated
// fixture, still does.
func runGopls(t *testing.T, cacheDir, root, posArg string) (string, error) {
	t.Helper()
	// The argv starts from this function's own "references" literal and
	// only ever grows by the caller's position argument, matching the
	// one already-audited exec.CommandContext pattern elsewhere in this
	// codebase (see handlers_codelens.go's execGenerate).
	args := []string{"references"}
	args = append(args, posArg)
	cmd := exec.Command("gopls", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), goplsCacheEnv(cacheDir)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runGoplsDefinitionJSON is runGopls for `definition`, the one subcommand
// this suite always passes -json to (see parseGoplsJSONSpans's doc for
// why).
func runGoplsDefinitionJSON(t *testing.T, cacheDir, root, posArg string) (string, error) {
	t.Helper()
	args := []string{"definition", "-json"}
	args = append(args, posArg)
	cmd := exec.Command("gopls", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), goplsCacheEnv(cacheDir)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runGoplsRename is runGopls for `rename`, whose argv shape differs from
// every other subcommand this suite drives: it always passes -w (apply the
// edit to disk, so its caller can read back the renamed files) and -l
// (list the files it touched), and takes a second positional argument (the
// new name) after posArg. Only externaltest_test.go still calls this
// (navaudit_parity2_test.go's own TestNavAudit_Rename applies its
// WorkspaceEdit in-memory instead, over the shared LSP session -- see
// goplsRenameEditsLSP's doc): externaltest_test.go's rename case needs a
// real on-disk tree to `go build`/`go vet` against, which -w gives it
// directly.
func runGoplsRename(t *testing.T, cacheDir, root, posArg, newName string) (string, error) {
	t.Helper()
	args := []string{"rename", "-w", "-l"}
	args = append(args, posArg, newName)
	cmd := exec.Command("gopls", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), goplsCacheEnv(cacheDir)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// goplsPosArg formats file (absolute, inside root) and pos (golance's
// 0-based LSP position) as gopls's <path>:<line>:<col> CLI argument,
// relative to root since that is gopls's working directory.
func goplsPosArg(t *testing.T, root, file string, pos protocol.Position) string {
	t.Helper()
	rel, err := filepath.Rel(root, file)
	if err != nil {
		t.Fatalf("filepath.Rel(%s, %s): %v", root, file, err)
	}
	return fmt.Sprintf("%s:%d:%d", rel, pos.Line+1, pos.Character+1)
}

// goplsJSONSpan mirrors the "span" object gopls's -json output embeds (see
// definition -json's {"span":{...},"description":...} shape).
type goplsJSONSpan struct {
	URI   string `json:"uri"`
	Start struct {
		Line   int `json:"line"`
		Column int `json:"column"`
	} `json:"start"`
	End struct {
		Line   int `json:"line"`
		Column int `json:"column"`
	} `json:"end"`
}

type goplsJSONResult struct {
	Span        *goplsJSONSpan `json:"span"`
	Description string         `json:"description"`
}

// parseGoplsJSONSpans accepts either a single {"span":...} object
// (definition -json's shape for one result) or a JSON array of such objects,
// and returns their locations.
func parseGoplsJSONSpans(t *testing.T, out string) ([]navLoc, error) {
	t.Helper()
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	var one goplsJSONResult
	if err := json.Unmarshal([]byte(out), &one); err == nil && one.Span != nil {
		return []navLoc{jsonSpanToLoc(*one.Span)}, nil
	}
	var many []goplsJSONResult
	if err := json.Unmarshal([]byte(out), &many); err == nil {
		locs := make([]navLoc, 0, len(many))
		for _, r := range many {
			if r.Span != nil {
				locs = append(locs, jsonSpanToLoc(*r.Span))
			}
		}
		return locs, nil
	}
	return nil, fmt.Errorf("navaudit: could not parse gopls -json output: %s", out)
}

func jsonSpanToLoc(s goplsJSONSpan) navLoc {
	file := strings.TrimPrefix(s.URI, "file://")
	return navLoc{file: file, line: s.Start.Line - 1, col: s.Start.Column - 1, endLine: s.End.Line - 1, endCol: s.End.Column - 1}
}

// --- LSP-over-stdio driver: this suite's own oracle for every navaudit
// case (definition, typeDefinition, references, prepareRename, rename,
// implementation, call hierarchy, type hierarchy). ---

// stdioRWC adapts a subprocess's stdin/stdout pipes into the single
// io.ReadWriteCloser jsonrpc2's header framing wants; Close closes both
// ends so the child sees stdin EOF and stops holding stdout open.
type stdioRWC struct {
	io.ReadCloser
	io.WriteCloser
}

func (s stdioRWC) Close() error {
	werr := s.WriteCloser.Close()
	rerr := s.ReadCloser.Close()
	if werr != nil {
		return werr
	}
	return rerr
}

// goplsLSP is a live `gopls serve` subprocess driven directly over LSP.
type goplsLSP struct {
	cmd    *exec.Cmd
	client *jsonrpc2.SingleClient
	root   string
}

// newGoplsLSP launches `gopls serve` rooted at root and completes the
// initialize/initialized handshake, returning a session ready for
// requests. It takes no *testing.T: sharedGopls (this suite's only caller)
// builds the package's one shared session inside a sync.Once, outside any
// single test's lifetime, so failures are reported via the returned error
// instead of t.Fatalf.
func newGoplsLSP(cacheDir, root string) (*goplsLSP, error) {
	cmd := exec.Command("gopls", "serve")
	cmd.Env = append(os.Environ(), goplsCacheEnv(cacheDir)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("gopls serve StdinPipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("gopls serve StdoutPipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("gopls serve start: %w", err)
	}

	stream := jsonrpc2.NewHeaderStream(stdioRWC{ReadCloser: stdout, WriteCloser: stdin})
	client, err := jsonrpc2.NewSingleClient(stream)
	if err != nil {
		return nil, fmt.Errorf("jsonrpc2.NewSingleClient: %w", err)
	}

	g := &goplsLSP{cmd: cmd, client: client, root: root}
	if err := g.initialize(); err != nil {
		return nil, err
	}
	return g, nil
}

// close shuts the session down: shutdown/exit over LSP, then waits for the
// child to exit, killing it if it does not within 10s. Only that timeout
// case is reported as an error -- gopls exiting from our own Notify(exit)
// races harmlessly against g.client.Close() tearing down its stdio pipes
// right after, which routinely surfaces as a broken-pipe Wait() error and
// is not a real failure.
func (g *goplsLSP) close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = g.client.Call(ctx, protocol.MethodShutdown, nil, nil)
	_ = g.client.Notify(ctx, protocol.MethodExit, nil)
	_ = g.client.Close()
	done := make(chan error, 1)
	go func() { done <- g.cmd.Wait() }()
	select {
	case <-done:
		return nil
	case <-time.After(10 * time.Second):
		_ = g.cmd.Process.Kill()
		<-done
		return fmt.Errorf("gopls serve did not exit after shutdown; killed")
	}
}

// initialize declares this suite's client capabilities -- workspace
// folder rooted at g.root, plus the navigation features it drives that
// have their own capability shape (typeHierarchy, typeDefinition,
// callHierarchy, rename's prepareSupport) -- and completes the
// initialize/initialized handshake. Capabilities are otherwise left
// minimal and workspace/configuration support is not declared, so gopls
// has no reason to make a server-to-client request this suite's
// SingleClient (which only ever reads its own call's matching response,
// silently skipping anything else -- see go.lsp.dev/jsonrpc2's
// SyncClient.Call) could never answer. linkSupport and
// workspaceEdit.documentChanges are also left undeclared, so gopls answers
// definition/references/implementation as Location|Location[] (never
// LocationLink[]) and rename as a plain "changes" map (never
// "documentChanges") -- the shapes this suite's decoders below assume.
func (g *goplsLSP) initialize() error {
	params := &protocol.InitializeParams{
		WorkspaceFoldersInitializeParams: protocol.WorkspaceFoldersInitializeParams{
			WorkspaceFolders: protocol.NewNullable([]protocol.WorkspaceFolder{{
				URI:  uri.File(g.root),
				Name: filepath.Base(g.root),
			}}),
		},
		Capabilities: protocol.ClientCapabilities{
			TextDocument: &protocol.TextDocumentClientCapabilities{
				TypeHierarchy:  &protocol.TypeHierarchyClientCapabilities{},
				TypeDefinition: &protocol.TypeDefinitionClientCapabilities{},
				CallHierarchy:  &protocol.CallHierarchyClientCapabilities{},
				Rename:         &protocol.RenameClientCapabilities{},
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var result protocol.InitializeResult
	if _, err := g.client.Call(ctx, protocol.MethodInitialize, params, &result); err != nil {
		return fmt.Errorf("gopls initialize: %w", err)
	}
	if err := g.client.Notify(ctx, protocol.MethodInitialized, &protocol.InitializedParams{}); err != nil {
		return fmt.Errorf("gopls initialized: %w", err)
	}
	return nil
}

// didOpen sends textDocument/didOpen for file, so gopls has the same view of
// it an editor opening the file would -- needed since some gopls analyses
// are keyed off open documents rather than the on-disk workspace scan alone.
func (g *goplsLSP) didOpen(file string) error {
	data, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := g.client.Notify(ctx, protocol.MethodTextDocumentDidOpen, &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        uri.File(file),
			LanguageID: "go",
			Version:    1,
			Text:       string(data),
		},
	}); err != nil {
		return fmt.Errorf("gopls didOpen %s: %w", file, err)
	}
	return nil
}

// --- the package's single shared gopls serve session ---

var (
	sharedGoplsOnce     sync.Once
	sharedGoplsInst     *goplsLSP
	errSharedGopls      error
	sharedGoplsCacheDir string
)

// navauditFilePaths lists every .go file (including _test.go) under
// testdata/module/navaudit, for sharedGopls's startup didOpen calls.
func navauditFilePaths(root string) ([]string, error) {
	base := filepath.Join(root, "navaudit")
	var out []string
	err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", base, err)
	}
	sort.Strings(out)
	return out, nil
}

// sharedGopls returns the package's single long-lived `gopls serve`
// session, started lazily on the first TestNavAudit_* call that needs one
// and shared by every case in every one of this file's tests, instead of
// either a fresh `gopls <subcommand>` process per case (~100 cold starts
// across Definition/References/PrepareRename/Implementation/CallHierarchy/
// Rename, each paying gopls's full workspace load -- this suite's
// dominant cost, dwarfing any one request) or the two independent
// sessions TestNavAudit_TypeDefinition and TestNavAudit_TypeHierarchy used
// to start on their own. TestMain closes it once, after every test in this
// binary has run (see closeSharedGopls).
func sharedGopls(t *testing.T, root string) *goplsLSP {
	t.Helper()
	sharedGoplsOnce.Do(func() {
		sharedGoplsInst, sharedGoplsCacheDir, errSharedGopls = startSharedGopls(root)
	})
	if errSharedGopls != nil {
		t.Fatalf("start shared gopls session: %v", errSharedGopls)
	}
	return sharedGoplsInst
}

// startSharedGopls does sharedGopls's actual work: create its cache
// directory, launch and initialize the session, and open every navaudit
// file. Kept t-free (unlike every other helper in this file) so it never
// has a *testing.T for os.MkdirTemp to shadow: sharedGoplsOnce.Do runs this
// exactly once, on whichever test happens to be first, so its cache
// directory must outlive that one test's own t.TempDir() -- through every
// other test in this binary, until TestMain's closeSharedGopls removes it.
func startSharedGopls(root string) (g *goplsLSP, cacheDir string, err error) {
	cacheDir, err = os.MkdirTemp("", "golance-navaudit-gopls-")
	if err != nil {
		return nil, "", fmt.Errorf("create shared gopls cache dir: %w", err)
	}
	g, err = newGoplsLSP(cacheDir, root)
	if err != nil {
		return nil, cacheDir, err
	}
	files, err := navauditFilePaths(root)
	if err != nil {
		return nil, cacheDir, err
	}
	for _, f := range files {
		if err := g.didOpen(f); err != nil {
			return nil, cacheDir, err
		}
	}
	return g, cacheDir, nil
}

// closeSharedGopls shuts the shared session down, if one was ever started,
// and removes its cache directory. Called once from TestMain, after every
// test in this binary has run.
func closeSharedGopls() {
	if sharedGoplsInst != nil {
		if err := sharedGoplsInst.close(); err != nil {
			fmt.Fprintf(os.Stderr, "navaudit: close shared gopls session: %v\n", err)
		}
	}
	if sharedGoplsCacheDir != "" {
		_ = os.RemoveAll(sharedGoplsCacheDir)
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	closeSharedGopls()
	os.Exit(code)
}

// --- typed request/response helpers, one per navigation feature this
// suite compares against golance. ---

// decodeLocationResult decodes a raw textDocument/definition,
// textDocument/references, textDocument/implementation, or
// textDocument/typeDefinition JSON result shaped as null, a single
// Location, or a Location array (gopls never returns LocationLink here
// since linkSupport was not declared in initialize).
func decodeLocationResult(t *testing.T, raw json.RawMessage) []navLoc {
	t.Helper()
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var multi []protocol.Location
		if err := json.Unmarshal(raw, &multi); err != nil {
			t.Fatalf("decoding Location[] result: %v (%s)", err, trimmed)
		}
		return locsFromLSP(multi)
	}
	var single protocol.Location
	if err := json.Unmarshal(raw, &single); err != nil {
		t.Fatalf("decoding Location result: %v (%s)", err, trimmed)
	}
	return []navLoc{locFromLSP(single)}
}

// locationRequest issues method (one of
// textDocument/definition|references|implementation, each answering
// Location | Location[] | null since this suite never declares
// linkSupport) at (file, pos) against g and decodes the result via
// decodeLocationResult.
func (g *goplsLSP) locationRequest(t *testing.T, method string, params any) []navLoc {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var raw json.RawMessage
	if _, err := g.client.Call(ctx, method, params, &raw); err != nil {
		t.Fatalf("gopls %s: %v", method, err)
	}
	return decodeLocationResult(t, raw)
}

// definition calls textDocument/definition.
func (g *goplsLSP) definition(t *testing.T, file string, pos protocol.Position) []navLoc {
	t.Helper()
	return g.locationRequest(t, protocol.MethodTextDocumentDefinition, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	})
}

// references calls textDocument/references with includeDeclaration:false,
// matching this suite's own golance-side calls.
func (g *goplsLSP) references(t *testing.T, file string, pos protocol.Position) []navLoc {
	t.Helper()
	return g.locationRequest(t, protocol.MethodTextDocumentReferences, &protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
		Context: protocol.ReferenceContext{IncludeDeclaration: false},
	})
}

// implementation calls textDocument/implementation.
func (g *goplsLSP) implementation(t *testing.T, file string, pos protocol.Position) []navLoc {
	t.Helper()
	return g.locationRequest(t, protocol.MethodTextDocumentImplementation, &protocol.ImplementationParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	})
}

// typeDefinitionOrErr calls textDocument/typeDefinition and returns gopls's
// own protocol error, if any, instead of failing the test: unlike golance's
// handleTypeDefinition (which always answers an empty result, never an LSP
// error, for "no named type here"), gopls answers a genuine JSON-RPC error
// for some positions (e.g. a zero- or multi-result function/method
// identifier -- see ./audit-navigation.md's typeDefinition findings), which
// callers that expect that need to tell apart from a routine empty result.
func (g *goplsLSP) typeDefinitionOrErr(t *testing.T, file string, pos protocol.Position) ([]navLoc, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Decoded as raw JSON, not protocol.TypeDefinitionResult, since that
	// union type's generated decoder cannot represent a JSON null result
	// (the routine "no type at this position" answer) at all -- it errors
	// with "cannot derive concrete type for nil interface with finite type
	// set" instead of leaving the interface nil.
	var raw json.RawMessage
	if _, err := g.client.Call(ctx, protocol.MethodTextDocumentTypeDefinition, &protocol.TypeDefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}, &raw); err != nil {
		return nil, err
	}
	return decodeLocationResult(t, raw), nil
}

// prepareRename calls textDocument/prepareRename and reports whether gopls
// considers the position renameable, returning its Range when it does.
// Decoded from raw JSON via an explicit key probe rather than
// protocol.PrepareRenameResult, for the same reason as typeDefinitionOrErr:
// go.lsp.dev/jsonrpc2's plain json.Unmarshal never wires in protocol's
// generated union decoder, and this needs to tell a genuine null (not
// renameable) apart from a decode error.
func (g *goplsLSP) prepareRename(t *testing.T, file string, pos protocol.Position) (*navLoc, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var raw json.RawMessage
	if _, err := g.client.Call(ctx, protocol.MethodTextDocumentPrepareRename, &protocol.PrepareRenameParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}, &raw); err != nil {
		return nil, false
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, false
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("decoding PrepareRename result: %v (%s)", err, trimmed)
	}
	if _, ok := keys["placeholder"]; ok {
		var p protocol.PrepareRenamePlaceholder
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decoding PrepareRenamePlaceholder result: %v (%s)", err, trimmed)
		}
		loc := rangeToNavLoc(file, p.Range)
		return &loc, true
	}
	if _, ok := keys["start"]; ok {
		var rng protocol.Range
		if err := json.Unmarshal(raw, &rng); err != nil {
			t.Fatalf("decoding Range result: %v (%s)", err, trimmed)
		}
		loc := rangeToNavLoc(file, rng)
		return &loc, true
	}
	t.Fatalf("gopls prepareRename: unrecognized result shape: %s", trimmed)
	return nil, false
}

// wireTextDocumentEdit mirrors one element of a WorkspaceEdit's
// "documentChanges" array in the shape gopls actually sends for a rename:
// a plain TextDocumentEdit (never the CreateFile/RenameFile/DeleteFile
// alternatives DocumentChange's other members represent -- a rename never
// needs those), whose own "edits" are plain TextEdits (never
// AnnotatedTextEdit or SnippetTextEdit, both gated behind client
// capabilities this suite's initialize never declares). Decoded through
// this concrete struct, not protocol.TextDocumentEdit itself, because its
// Edits field is typed as the union TextDocumentEditElement, and
// go.lsp.dev/jsonrpc2's plain json.Unmarshal never wires in protocol's
// generated union decoder (see prepareRename's doc for the same issue).
type wireTextDocumentEdit struct {
	TextDocument struct {
		URI uri.URI `json:"uri"`
	} `json:"textDocument"`
	Edits []protocol.TextEdit `json:"edits"`
}

// rename calls textDocument/rename and normalizes its result -- gopls
// answers with "documentChanges" (a TextDocumentEdit per file, itself
// gated on nothing this suite's initialize would need to opt into: the LSP
// spec makes it a server's own choice whenever it needs per-document
// versioning, not conditional on client capabilities) rather than the
// plain "changes" map protocol.WorkspaceEdit.Changes alone would decode --
// into a Changes-only *protocol.WorkspaceEdit, so every caller (starting
// with goplsRenameEditsLSP) can keep reading edit.Changes exactly like
// golanceRenameEdits already does for golance's own result.
func (g *goplsLSP) rename(t *testing.T, file string, pos protocol.Position, newName string) *protocol.WorkspaceEdit {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var raw json.RawMessage
	if _, err := g.client.Call(ctx, protocol.MethodTextDocumentRename, &protocol.RenameParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
		NewName: newName,
	}, &raw); err != nil {
		t.Fatalf("gopls textDocument/rename: %v", err)
	}
	var wire struct {
		Changes         map[uri.URI][]protocol.TextEdit `json:"changes"`
		DocumentChanges []wireTextDocumentEdit          `json:"documentChanges"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decoding WorkspaceEdit result: %v (%s)", err, raw)
	}
	edit := &protocol.WorkspaceEdit{Changes: map[uri.URI][]protocol.TextEdit{}}
	for u, edits := range wire.Changes {
		edit.Changes[u] = append(edit.Changes[u], edits...)
	}
	for _, dc := range wire.DocumentChanges {
		edit.Changes[dc.TextDocument.URI] = append(edit.Changes[dc.TextDocument.URI], dc.Edits...)
	}
	return edit
}

// prepareCallHierarchy calls textDocument/prepareCallHierarchy.
func (g *goplsLSP) prepareCallHierarchy(t *testing.T, file string, pos protocol.Position) []protocol.CallHierarchyItem {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var items []protocol.CallHierarchyItem
	if _, err := g.client.Call(ctx, protocol.MethodTextDocumentPrepareCallHierarchy, &protocol.CallHierarchyPrepareParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}, &items); err != nil {
		t.Fatalf("gopls textDocument/prepareCallHierarchy: %v", err)
	}
	return items
}

// incomingCalls and outgoingCalls take item by pointer, not value: like
// relatedTypeHierarchy in production code and this file's own
// supertypes/subtypes, this avoids copying the CallHierarchyItem
// (gocritic's hugeParam).
func (g *goplsLSP) incomingCalls(t *testing.T, item *protocol.CallHierarchyItem) []protocol.CallHierarchyIncomingCall {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var calls []protocol.CallHierarchyIncomingCall
	if _, err := g.client.Call(ctx, protocol.MethodCallHierarchyIncomingCalls, &protocol.CallHierarchyIncomingCallsParams{Item: *item}, &calls); err != nil {
		t.Fatalf("gopls callHierarchy/incomingCalls: %v", err)
	}
	return calls
}

func (g *goplsLSP) outgoingCalls(t *testing.T, item *protocol.CallHierarchyItem) []protocol.CallHierarchyOutgoingCall {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var calls []protocol.CallHierarchyOutgoingCall
	if _, err := g.client.Call(ctx, protocol.MethodCallHierarchyOutgoingCalls, &protocol.CallHierarchyOutgoingCallsParams{Item: *item}, &calls); err != nil {
		t.Fatalf("gopls callHierarchy/outgoingCalls: %v", err)
	}
	return calls
}

// prepareTypeHierarchy calls textDocument/prepareTypeHierarchy.
func (g *goplsLSP) prepareTypeHierarchy(t *testing.T, file string, pos protocol.Position) []protocol.TypeHierarchyItem {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var items []protocol.TypeHierarchyItem
	if _, err := g.client.Call(ctx, protocol.MethodTextDocumentPrepareTypeHierarchy, &protocol.TypeHierarchyPrepareParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}, &items); err != nil {
		t.Fatalf("gopls textDocument/prepareTypeHierarchy: %v", err)
	}
	return items
}

// supertypes and subtypes take item by pointer, not value: like
// relatedTypeHierarchy in production code, this avoids copying the
// 128-byte protocol.TypeHierarchyItem (gocritic's hugeParam).
func (g *goplsLSP) supertypes(t *testing.T, item *protocol.TypeHierarchyItem) []protocol.TypeHierarchyItem {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var items []protocol.TypeHierarchyItem
	if _, err := g.client.Call(ctx, protocol.MethodTypeHierarchySupertypes, &protocol.TypeHierarchySupertypesParams{Item: *item}, &items); err != nil {
		t.Fatalf("gopls typeHierarchy/supertypes: %v", err)
	}
	return items
}

func (g *goplsLSP) subtypes(t *testing.T, item *protocol.TypeHierarchyItem) []protocol.TypeHierarchyItem {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var items []protocol.TypeHierarchyItem
	if _, err := g.client.Call(ctx, protocol.MethodTypeHierarchySubtypes, &protocol.TypeHierarchySubtypesParams{Item: *item}, &items); err != nil {
		t.Fatalf("gopls typeHierarchy/subtypes: %v", err)
	}
	return items
}

func typeHierarchyItemNames(items []protocol.TypeHierarchyItem) []string {
	names := make([]string, len(items))
	for i := range items {
		names[i] = items[i].Name
	}
	sort.Strings(names)
	return names
}
