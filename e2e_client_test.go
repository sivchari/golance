package golance_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

const jsonrpcVersion = "2.0"

// message is the wire-level JSON-RPC 2.0 envelope, matching
// internal/rpc.message: Method set means a request (ID present) or
// notification (ID absent); Result/Error set means a response to one of the
// client's own requests.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// diagnosticsNotification is one textDocument/publishDiagnostics
// notification, decoded and queued by readLoop for waitForDiagnostics.
type diagnosticsNotification struct {
	uri   string
	diags []protocol.Diagnostic
}

// progressNotification is one $/progress notification's token, kind
// ("begin", "report", or "end"), and message, decoded and queued by
// readLoop for waitForIndexReady. For an "end" notification, message is
// the indexer subprocess's build-stats summary (see internal/server's
// indexStatsMessage), when present.
type progressNotification struct {
	token   string
	kind    string
	message string
}

// serverRequest is one server-initiated JSON-RPC request (e.g.
// workspace/inlayHint/refresh), timestamped at the moment readLoop decoded
// it, for latency measurements that need to know when the server asked the
// client for something rather than only when the client asked the server.
// readLoop answers every such request with a null result immediately after
// queuing it, so the server's own s.rpc.Request never blocks waiting on a
// reply this minimal client has no other reason to send.
type serverRequest struct {
	method     string
	receivedAt time.Time
}

// lspClient is a minimal stdio LSP client good enough to drive golance: it
// correlates responses by id and queues the two notification kinds the E2E
// suite waits on (diagnostics, index-build progress) on their own channels
// so a wait for one kind never consumes a notification a later subtest
// needs.
type lspClient struct {
	cmd *exec.Cmd
	in  *frameWriter
	out *bufio.Reader

	mu      sync.Mutex
	seq     int
	pending map[string]chan *message

	diagnostics chan diagnosticsNotification
	progress    chan progressNotification
	requests    chan serverRequest
}

var (
	e2eBuildOnce sync.Once
	e2eBuildBin  string
	errE2EBuild  error

	e2eGocacheOnce sync.Once
	e2eGocacheDir  string
	errE2EGocache  error
)

// mkdirTempProcessWide creates a temp directory scoped to the whole test
// process rather than to a single test (see e2eGocache and
// buildGolanceBinary, both memoized process-wide via sync.Once). It
// deliberately does not use t.TempDir(), whose cleanup runs as soon as the
// calling test and its subtests finish — which would remove the directory
// out from under every later test still reusing the memoized result.
func mkdirTempProcessWide(pattern string) (string, error) {
	return os.MkdirTemp("", pattern)
}

// e2eGocache returns a GOCACHE directory dedicated to this test process,
// created once and reused by every subtest: golance's own build (via
// buildGolanceBinary) and every synthetic workspace's `go list -export`
// calls share it. Isolating GOCACHE this way (rather than pointing at the
// developer's real one, as e2eEnv otherwise would) keeps two things out of
// their real build cache: object code for the disposable synthetic
// packages e2e_repo_test.go generates, and repeated identical-content
// builds across t.TempDir() runs that would otherwise collide on the same
// build-cache action IDs run after run.
func e2eGocache(t *testing.T) string {
	t.Helper()
	e2eGocacheOnce.Do(func() {
		e2eGocacheDir, errE2EGocache = mkdirTempProcessWide("golance-e2e-gocache")
	})
	if errE2EGocache != nil {
		t.Fatalf("create e2e GOCACHE: %v", errE2EGocache)
	}
	return e2eGocacheDir
}

// buildGolanceBinary builds ./cmd/golance once per test process. The output
// path is a constant relative to the repository root (kept in .gitignore)
// so every exec.Command argument is a literal; concurrent `go test` runs on
// the same checkout share the same path, which is safe because the binary
// content is identical for identical sources.
func buildGolanceBinary(t *testing.T) string {
	t.Helper()
	e2eBuildOnce.Do(func() {
		cmd := exec.Command("go", "build", "-o", "e2e-bin/golance-e2e", "./cmd/golance")
		out, err := cmd.CombinedOutput()
		if err != nil {
			errE2EBuild = fmt.Errorf("go build ./cmd/golance: %w\n%s", err, out)
			return
		}
		abs, err := filepath.Abs(filepath.Join("e2e-bin", "golance-e2e"))
		if err != nil {
			errE2EBuild = err
			return
		}
		e2eBuildBin = abs
	})
	if errE2EBuild != nil {
		t.Fatalf("build golance: %v", errE2EBuild)
	}
	return e2eBuildBin
}

// startClient builds and starts golance for the given workspace root with an
// isolated HOME/XDG_CACHE_HOME, so the server's own graph and facts-index
// caches (internal/graph.CacheFile, internal/server.indexDBFile) never leak
// into the developer's real cache directory.
//
// golance runs in its own process group (Setpgid) so teardown can kill it
// together with the indexer subprocess it launches; both are SIGKILLed and
// reaped before the fake home is removed, with errors ignored (an indexer
// still exiting could otherwise race the cleanup and fail the test with
// "directory not empty").
func startClient(t *testing.T, root string) *lspClient {
	t.Helper()
	return startClientIn(t, root, t.TempDir())
}

// startClientIn is startClient with the fake HOME supplied by the caller
// instead of created fresh, so two clients can share one HOME/XDG_CACHE_HOME
// — e.g. two golance sessions for different git worktrees of the same
// repository, which must resolve to the same shared facts database (see
// internal/server.indexDBFile) exactly as two real editor sessions on one
// machine would. Unlike startClient, it does not remove fakeHome itself
// (t.Cleanup order is LIFO, so a caller sharing one fakeHome across several
// startClientIn calls must register its own removal before the first call,
// so that cleanup runs last — after every client's own kill-and-log-dump
// cleanup below has already run against fakeHome's still-intact contents).
func startClientIn(t *testing.T, root, fakeHome string) *lspClient {
	t.Helper()
	bin := buildGolanceBinary(t)
	stderrPath := filepath.Join(fakeHome, "golance.stderr")

	cmd := runGolance(bin)
	cmd.Dir = root
	cmd.Env = e2eEnv(t, fakeHome)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderrFile, err := os.Create(filepath.Clean(stderrPath))
	if err != nil {
		t.Fatalf("create stderr log: %v", err)
	}
	cmd.Stderr = stderrFile
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start golance: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = cmd.Wait()
		_ = stderrFile.Close()
		if t.Failed() {
			logFileContent(t, "golance stderr (server logs go here by default; no -log flag)", stderrPath)
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

// runGolance starts bin with no arguments. bin is a function parameter, so
// gosec's "subprocess launched with variable" check exempts it as the
// executable name (its own rule carves out parameters/receivers used in
// that position); golance defaults its log output to stderr when -log is
// not given, which startClientIn already captures separately, so there is
// no second, dynamically-named argument left to trip the same check.
func runGolance(bin string) *exec.Cmd {
	return exec.Command(bin)
}

// e2eEnv isolates the child process. HOME/XDG_CACHE_HOME point at a fake
// home so golance's own caches stay inside it; GOCACHE points at this test
// process's dedicated e2eGocache dir (see its doc for why); GOPATH and
// GOMODCACHE are pinned to their real values because the fake HOME would
// otherwise reroute their defaults too, and go/packages.Load (which golance
// shells out to `go list` through) needs the real module cache to stay fast
// and avoid network access.
func e2eEnv(t *testing.T, fakeHome string) []string {
	t.Helper()
	cache := filepath.Join(fakeHome, ".cache")
	if err := os.MkdirAll(cache, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", cache, err)
	}
	overrides := map[string]string{
		"HOME":           fakeHome,
		"XDG_CACHE_HOME": cache,
		"GOCACHE":        e2eGocache(t),
	}
	if v := realGoPath(); v != "" {
		overrides["GOPATH"] = v
	}
	if v := realGoModCache(); v != "" {
		overrides["GOMODCACHE"] = v
	}
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if _, ok := overrides[key]; ok {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	return env
}

// realGoPath returns the developer's real GOPATH, or "" if it cannot be
// determined.
func realGoPath() string {
	out, err := exec.Command("go", "env", "GOPATH").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// realGoModCache returns the developer's real GOMODCACHE, or "" if it
// cannot be determined.
func realGoModCache() string {
	out, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func logFileContent(t *testing.T, label, path string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil || len(b) == 0 {
		return
	}
	t.Logf("%s (%s):\n%s", label, path, b)
}

// readLoop dispatches frames from golance's stdout: responses to the
// client's own requests are routed to their pending channel by id;
// publishDiagnostics and $/progress notifications are queued on their own
// channels; every other notification (window/showMessage, ...) is dropped.
func (c *lspClient) readLoop() {
	for {
		raw, err := readFrame(c.out)
		if err != nil {
			return
		}
		var m message
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		switch {
		case m.Method == "" && m.ID != nil:
			c.mu.Lock()
			ch := c.pending[string(m.ID)]
			delete(c.pending, string(m.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- &m
			}
		case m.Method == protocol.MethodTextDocumentPublishDiagnostics:
			c.dispatchDiagnostics(&m)
		case m.Method == protocol.MethodProgress:
			c.dispatchProgress(&m)
		case m.Method != "" && m.ID != nil:
			c.dispatchServerRequest(&m)
		}
	}
}

// stop sends the LSP shutdown/exit sequence and waits for the process to
// exit, so any exclusive lock it held on a facts database (see
// internal/store's bbolt-backed DB, held for a session's whole lifetime) is
// released before a caller starts another session against the same
// database — mirroring a user closing one editor window before opening
// another. startClientIn's own t.Cleanup still runs at test end regardless
// (Wait/Kill on an already-exited process are harmless no-ops there).
func (c *lspClient) stop(t *testing.T) {
	t.Helper()
	resp := c.call(t, protocol.MethodShutdown, nil, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("shutdown failed: %s", resp.Error)
	}
	c.notify(t, "exit", nil)
	done := make(chan struct{})
	go func() { _ = c.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(e2eRequestBudget):
		t.Fatalf("golance did not exit within %s after shutdown/exit", e2eRequestBudget)
	}
}

func (c *lspClient) dispatchDiagnostics(m *message) {
	var p protocol.PublishDiagnosticsParams
	if protocol.Unmarshal(m.Params, &p) != nil {
		return
	}
	c.diagnostics <- diagnosticsNotification{uri: string(p.URI), diags: p.Diagnostics}
}

func (c *lspClient) dispatchProgress(m *message) {
	var p protocol.ProgressParams
	if protocol.Unmarshal(m.Params, &p) != nil {
		return
	}
	token, _ := p.Token.(protocol.String)
	var kv struct {
		Kind    string  `json:"kind"`
		Message *string `json:"message"`
	}
	_ = json.Unmarshal(p.Value, &kv)
	var msg string
	if kv.Message != nil {
		msg = *kv.Message
	}
	c.progress <- progressNotification{token: string(token), kind: kv.Kind, message: msg}
}

// dispatchServerRequest records m's arrival time and answers it with a null
// result, so a server-initiated request this minimal client has no
// domain-specific handling for (e.g. workspace/inlayHint/refresh) never
// blocks the server's own s.rpc.Request waiting on a reply.
func (c *lspClient) dispatchServerRequest(m *message) {
	select {
	case c.requests <- serverRequest{method: m.Method, receivedAt: time.Now()}:
	default:
	}
	raw, err := json.Marshal(message{JSONRPC: jsonrpcVersion, ID: m.ID, Result: json.RawMessage("null")})
	if err != nil {
		return
	}
	_ = c.in.write(raw)
}

// call sends a request and waits for its response, failing t on timeout.
func (c *lspClient) call(t *testing.T, method string, params any, timeout time.Duration) *message {
	t.Helper()
	b, err := protocol.Marshal(params)
	if err != nil {
		t.Fatalf("marshal %s params: %v", method, err)
	}
	c.mu.Lock()
	c.seq++
	id := json.RawMessage(strconv.Quote(fmt.Sprintf("e2e-%d", c.seq)))
	ch := make(chan *message, 1)
	c.pending[string(id)] = ch
	c.mu.Unlock()

	raw, err := json.Marshal(message{JSONRPC: jsonrpcVersion, ID: id, Method: method, Params: b})
	if err != nil {
		t.Fatalf("marshal %s request: %v", method, err)
	}
	if err := c.in.write(raw); err != nil {
		t.Fatalf("write %s request: %v", method, err)
	}

	select {
	case m := <-ch:
		return m
	case <-time.After(timeout):
		c.mu.Lock()
		delete(c.pending, string(id))
		c.mu.Unlock()
		t.Fatalf("%s timed out after %s", method, timeout)
		return nil
	}
}

func (c *lspClient) notify(t *testing.T, method string, params any) {
	t.Helper()
	b, err := protocol.Marshal(params)
	if err != nil {
		t.Fatalf("marshal %s params: %v", method, err)
	}
	raw, err := json.Marshal(message{JSONRPC: jsonrpcVersion, Method: method, Params: b})
	if err != nil {
		t.Fatalf("marshal %s notification: %v", method, err)
	}
	if err := c.in.write(raw); err != nil {
		t.Fatalf("write %s notification: %v", method, err)
	}
}

// initialize performs the LSP handshake and returns the decoded
// InitializeResult.
func (c *lspClient) initialize(t *testing.T, root string) *protocol.InitializeResult {
	t.Helper()
	gopid := os.Getpid()
	if gopid < 0 || gopid > math.MaxInt32 {
		t.Fatalf("pid %d does not fit in int32", gopid)
		return nil // unreachable: t.Fatalf halts this goroutine via runtime.Goexit
	}
	pid := int32(gopid)
	params := &protocol.InitializeParams{
		ProcessID: &pid,
		WorkspaceFoldersInitializeParams: protocol.WorkspaceFoldersInitializeParams{
			WorkspaceFolders: protocol.NewNullable([]protocol.WorkspaceFolder{
				{URI: uri.File(root), Name: filepath.Base(root)},
			}),
		},
	}
	resp := c.call(t, protocol.MethodInitialize, params, e2eIndexBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("initialize failed: %s", resp.Error)
	}
	var result protocol.InitializeResult
	if err := protocol.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal initialize result: %v", err)
	}
	c.notify(t, protocol.MethodInitialized, &protocol.InitializedParams{})
	return &result
}

// openFile sends textDocument/didOpen with the file's on-disk content.
func (c *lspClient) openFile(t *testing.T, path string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	c.notify(t, protocol.MethodTextDocumentDidOpen, &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        uri.File(path),
			LanguageID: protocol.LanguageKindGo,
			Version:    1,
			Text:       string(content),
		},
	})
}

// openNewFile sends textDocument/didOpen with text, without ever reading or
// writing path on disk — simulating a brand-new file an editor just
// created and has not saved yet.
func (c *lspClient) openNewFile(t *testing.T, path, text string) {
	t.Helper()
	c.notify(t, protocol.MethodTextDocumentDidOpen, &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        uri.File(path),
			LanguageID: protocol.LanguageKindGo,
			Version:    1,
			Text:       text,
		},
	})
}

// changeFile sends textDocument/didChange replacing path's whole content
// with newText, without saving it to disk.
func (c *lspClient) changeFile(t *testing.T, path string, version int32, newText string) {
	t.Helper()
	c.notify(t, protocol.MethodTextDocumentDidChange, &protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: uri.File(path)},
			Version:                version,
		},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{
			&protocol.TextDocumentContentChangeWholeDocument{Text: newText},
		},
	})
}

// waitForDiagnostics blocks until a publishDiagnostics notification for path
// arrives, or e2eRequestBudget elapses.
func (c *lspClient) waitForDiagnostics(t *testing.T, path string) []protocol.Diagnostic {
	t.Helper()
	want := string(uri.File(path))
	deadline := time.After(e2eRequestBudget)
	for {
		select {
		case n := <-c.diagnostics:
			if n.uri == want {
				return n.diags
			}
		case <-deadline:
			t.Fatalf("no publishDiagnostics for %s within %s", path, e2eRequestBudget)
			return nil
		}
	}
}

// waitForIndexReady blocks until the indexer subprocess's "golance/index"
// $/progress end notification arrives, or e2eIndexBudget elapses, and
// returns its message (see internal/server's indexStatsMessage) — the
// build-stats summary a caller can parse via indexStats instead of
// inferring what happened from wall-clock time. Cross-reference queries
// (definition, references, implementation, workspace/symbol, rename)
// answer empty results until this fires (see internal/server.resolverOrWarn).
func (c *lspClient) waitForIndexReady(t *testing.T) string {
	t.Helper()
	const token = "golance/index"
	deadline := time.After(e2eIndexBudget)
	for {
		select {
		case n := <-c.progress:
			if n.token == token && n.kind == "end" {
				return n.message
			}
		case <-deadline:
			t.Fatalf("index did not become ready within %s", e2eIndexBudget)
			return ""
		}
	}
}

// indexStats is the build-stats summary parsed from waitForIndexReady's
// return value (see internal/server's indexStatsMessage, the producer of
// the exact format this expects).
type indexStats struct {
	typeChecked, casHits, unchanged, errors int
}

// parseIndexStats parses msg (waitForIndexReady's return value) into an
// indexStats, failing t if msg is not in the expected format — e.g. empty,
// which means the indexer subprocess exited before ever reaching its final
// STATS line (see runIndexer's exit-code contract in cmd/golance).
func parseIndexStats(t *testing.T, msg string) indexStats {
	t.Helper()
	var s indexStats
	if _, err := fmt.Sscanf(msg, "%d type-checked, %d resolved from cache, %d unchanged, %d error(s)", &s.typeChecked, &s.casHits, &s.unchanged, &s.errors); err != nil {
		t.Fatalf("parse index build stats %q: %v", msg, err)
	}
	return s
}

// waitForNonEmptyLocations polls method (a definition/references-shaped
// request) until it returns at least one location, or timeout elapses. The
// indexer subprocess's $/progress "end" notification (see
// waitForIndexReady) fires once it exits, but the server still has to open
// the resulting facts database and rebuild its Resolver before cross-
// reference queries answer (internal/server.buildIndex): a single request
// right after waitForIndexReady can legitimately race that short window.
func (c *lspClient) waitForNonEmptyLocations(t *testing.T, method string, params any, timeout time.Duration) protocol.LocationSlice {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		resp := c.call(t, method, params, e2eRequestBudget)
		if len(resp.Error) > 0 {
			t.Fatalf("%s failed: %s", method, resp.Error)
		}
		var got protocol.LocationSlice
		if err := protocol.Unmarshal(resp.Result, &got); err != nil {
			t.Fatalf("unmarshal %s result: %v", method, err)
		}
		if len(got) > 0 {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s returned no locations within %s of the index becoming ready", method, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// frameWriter writes Content-Length-framed JSON-RPC messages to golance's
// stdin, serialized so concurrent callers cannot interleave frames.
type frameWriter struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func newFrameWriter(w io.Writer) *frameWriter {
	return &frameWriter{w: bufio.NewWriterSize(w, 1<<20)}
}

func (fw *frameWriter) write(body []byte) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if _, err := fmt.Fprintf(fw.w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	if _, err := fw.w.Write(body); err != nil {
		return err
	}
	return fw.w.Flush()
}

// readFrame reads one Content-Length-delimited JSON-RPC message body from r,
// mirroring internal/rpc's own frame reader.
func readFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("e2e: invalid Content-Length header %q: %w", v, err)
		}
		length = n
	}
	if length < 0 {
		return nil, fmt.Errorf("e2e: missing Content-Length header")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}
