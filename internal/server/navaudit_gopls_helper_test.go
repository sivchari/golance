package server

// This file drives the real gopls v0.23.0 binary -- the oracle for the
// golance-vs-gopls navigation parity audit (see navaudit_parity_test.go and
// ../../audit-navigation.md) -- two ways: its CLI subcommands (definition,
// references, implementation, call_hierarchy, prepare_rename, rename), and,
// for the two navigation features gopls exposes no CLI subcommand for
// (typeDefinition, typeHierarchy/supertypes/subtypes), directly over LSP
// stdio using the same go.lsp.dev/jsonrpc2 and go.lsp.dev/protocol
// dependencies golance itself already uses to speak the protocol.

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
// os.UserCacheDir()) live outside the sandbox's writable set. cacheDir is
// one t.TempDir() shared by every gopls call within a single top-level
// TestNavAudit_* test (see that test's own cacheDir := t.TempDir()), so its
// later subtests reuse the first one's warm build cache instead of each
// paying a full cold load, while still getting real per-test cleanup.
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
// line:col-line:col (multi-line) range -- the format references,
// implementation, and prepare_rename each emit one such line per result.
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

// runGopls runs `gopls <subcommand> <posArg>` (a fresh process per call,
// matching how a shell user would drive it) against root, returning its
// combined output. A non-zero exit is not itself fatal -- "no identifier
// found" and similar routine misses exit non-zero -- callers decide what an
// error means for their own subcommand. cacheDir is the caller's shared
// t.TempDir() (see goplsCacheEnv's doc). Each argv below is a fixed-arity,
// individually-typed argument list, not a spread of a caller-supplied
// slice, matching the one already-audited exec.CommandContext pattern
// elsewhere in this codebase (see handlers_codelens.go's execGenerate) --
// gopls's own subcommand and flags are always this function's own literals.
func runGopls(t *testing.T, cacheDir, root, subcommand, posArg string) (string, error) {
	t.Helper()
	// The argv starts from this function's own literals and only ever
	// grows by the caller's position argument: the subcommand is matched
	// against the closed set this suite drives, never passed through.
	var cmd *exec.Cmd
	switch subcommand {
	case "references":
		args := []string{"references"}
		args = append(args, posArg)
		cmd = exec.Command("gopls", args...)
	case "prepare_rename":
		args := []string{"prepare_rename"}
		args = append(args, posArg)
		cmd = exec.Command("gopls", args...)
	case "implementation":
		args := []string{"implementation"}
		args = append(args, posArg)
		cmd = exec.Command("gopls", args...)
	case "call_hierarchy":
		args := []string{"call_hierarchy"}
		args = append(args, posArg)
		cmd = exec.Command("gopls", args...)
	default:
		t.Fatalf("runGopls: unsupported subcommand %q", subcommand)
		return "", nil
	}
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
// new name) after posArg.
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
// definition -json's {"span":{...},"description":...} shape); references
// and implementation's -json (where supported) emit a bare array of spans
// instead of wrapping each in an object with a "span" key, so
// parseGoplsJSONSpans handles both.
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

// callHierarchyLineRE matches one caller[N]/callee[N] line of gopls's
// call_hierarchy CLI output, e.g.:
//
//	caller[0]: ranges 41:9-19 in FILE from/to function callWithNamed in FILE:40:6-19
var callHierarchyLineRE = regexp.MustCompile(`^(caller|callee)\[\d+\]: ranges (\d+):(\d+)-(\d+) in (.+) from/to (?:function|method) (\S+) in (.+):(\d+):(\d+)-(\d+)$`)

// callHierarchyEdge is one caller[]/callee[] line: the identifier
// (function/method) at the other end, plus the call-site range within the
// origin item's own source.
type callHierarchyEdge struct {
	calleeOrCallerName string
	otherEnd           navLoc // the other function/method's own declaration
	callSite           navLoc // the call site's range, in the origin item's file
}

// parseGoplsCallHierarchy parses gopls call_hierarchy's plain-text output
// into its caller and callee edges.
func parseGoplsCallHierarchy(output string) (callers, callees []callHierarchyEdge) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		m := callHierarchyLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		callSiteLine, _ := strconv.Atoi(m[2])
		callSiteCol, _ := strconv.Atoi(m[3])
		callSiteEndCol, _ := strconv.Atoi(m[4])
		otherLine, _ := strconv.Atoi(m[8])
		otherCol, _ := strconv.Atoi(m[9])
		otherEndCol, _ := strconv.Atoi(m[10])
		edge := callHierarchyEdge{
			calleeOrCallerName: m[6],
			otherEnd:           locFromGoplsSpan(m[7], otherLine, otherCol, otherLine, otherEndCol),
			callSite:           locFromGoplsSpan(m[5], callSiteLine, callSiteCol, callSiteLine, callSiteEndCol),
		}
		if m[1] == "caller" {
			callers = append(callers, edge)
		} else {
			callees = append(callees, edge)
		}
	}
	return callers, callees
}

// --- LSP-over-stdio driver, for typeDefinition and typeHierarchy: gopls
// v0.23.0's CLI has no subcommand for either (see `gopls help`), so these
// are the only navaudit cases driven over raw LSP instead of the CLI. ---

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

// goplsLSP is a live `gopls serve` subprocess driven directly over LSP,
// used only for the two navigation features the CLI cannot answer.
type goplsLSP struct {
	cmd    *exec.Cmd
	client *jsonrpc2.SingleClient
	root   string
}

// startGoplsLSP launches `gopls serve` rooted at root, completes the
// initialize/initialized handshake, and registers a cleanup that shuts it
// down. Capabilities are left minimal and workspace/configuration support is
// not declared, so gopls has no reason to make a server-to-client request
// this test's SingleClient (which only ever reads its own call's matching
// response, silently skipping anything else -- see go.lsp.dev/jsonrpc2's
// SyncClient.Call) could never answer.
func startGoplsLSP(t *testing.T, cacheDir, root string) *goplsLSP {
	t.Helper()
	cmd := exec.Command("gopls", "serve")
	cmd.Env = append(os.Environ(), goplsCacheEnv(cacheDir)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("gopls serve StdinPipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("gopls serve StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("gopls serve start: %v", err)
	}

	stream := jsonrpc2.NewHeaderStream(stdioRWC{ReadCloser: stdout, WriteCloser: stdin})
	client, err := jsonrpc2.NewSingleClient(stream)
	if err != nil {
		t.Fatalf("jsonrpc2.NewSingleClient: %v", err)
	}

	g := &goplsLSP{cmd: cmd, client: client, root: root}
	t.Cleanup(func() { g.close(t) })
	g.initialize(t)
	return g
}

func (g *goplsLSP) close(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = g.client.Call(ctx, protocol.MethodShutdown, nil, nil)
	_ = g.client.Notify(ctx, protocol.MethodExit, nil)
	_ = g.client.Close()
	done := make(chan error, 1)
	go func() { done <- g.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = g.cmd.Process.Kill()
		<-done
	}
}

func (g *goplsLSP) initialize(t *testing.T) {
	t.Helper()
	rootURI := uri.File(g.root)
	params := &protocol.InitializeParams{
		RootURI: &rootURI,
		Capabilities: protocol.ClientCapabilities{
			TextDocument: &protocol.TextDocumentClientCapabilities{
				TypeHierarchy:  &protocol.TypeHierarchyClientCapabilities{},
				TypeDefinition: &protocol.TypeDefinitionClientCapabilities{},
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var result protocol.InitializeResult
	if _, err := g.client.Call(ctx, protocol.MethodInitialize, params, &result); err != nil {
		t.Fatalf("gopls initialize: %v", err)
	}
	if err := g.client.Notify(ctx, protocol.MethodInitialized, &protocol.InitializedParams{}); err != nil {
		t.Fatalf("gopls initialized: %v", err)
	}
}

// didOpen sends textDocument/didOpen for file, so gopls has the same view of
// it an editor opening the file would -- needed since some gopls analyses
// are keyed off open documents rather than the on-disk workspace scan alone.
func (g *goplsLSP) didOpen(t *testing.T, file string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = g.client.Notify(ctx, protocol.MethodTextDocumentDidOpen, &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        uri.File(file),
			LanguageID: "go",
			Version:    1,
			Text:       string(data),
		},
	})
	if err != nil {
		t.Fatalf("gopls didOpen %s: %v", file, err)
	}
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

// decodeLocationResult decodes a raw textDocument/typeDefinition or
// prepareTypeHierarchy-family JSON result shaped as null, a single
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
// relatedTypeHierarchy in production code and handlers_typehierarchy_test.go's
// own supertypesOf/subtypesOf, this avoids copying the 128-byte
// protocol.TypeHierarchyItem (gocritic's hugeParam).
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
