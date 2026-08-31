package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// frame encodes a JSON-RPC message body with a Content-Length header, for
// building test input streams.
func frame(t *testing.T, body string) string {
	t.Helper()
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
}

// readFrames decodes every Content-Length-framed message in b into raw JSON
// bodies, for asserting on Server output.
func readFrames(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	r := bufio.NewReader(bytes.NewReader(b))
	var out []map[string]any
	for {
		raw, err := readFrame(r)
		if err != nil {
			return out
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal frame: %v", err)
		}
		out = append(out, m)
	}
}

// frameForID returns the response frame with the given numeric id. Responses
// to requests handled on different goroutines/pools can be written in any
// relative order, so tests must look responses up by id rather than assume
// wire position.
func frameForID(t *testing.T, frames []map[string]any, id float64) map[string]any {
	t.Helper()
	for _, f := range frames {
		if got, ok := f["id"].(float64); ok && got == id {
			return f
		}
	}
	t.Fatalf("no frame with id=%v in %v", id, frames)
	return nil
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	logger := log.New(&testWriter{t}, "", 0)
	return NewServer(WithLogger(logger))
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

func TestServeDispatchesRequestAndWritesResult(t *testing.T) {
	s := newTestServer(t)
	s.Handle("initialize", Interactive, func(context.Context, json.RawMessage) (any, error) {
		return map[string]string{"ok": "true"}, nil
	})

	in := strings.NewReader(frame(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	var out bytes.Buffer
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	frames := readFrames(t, out.Bytes())
	if len(frames) != 1 {
		t.Fatalf("got %d response frames, want 1: %v", len(frames), frames)
	}
	result, _ := frames[0]["result"].(map[string]any)
	if result["ok"] != "true" {
		t.Fatalf("result = %v, want ok=true", frames[0]["result"])
	}
}

func TestUnknownMethodReturnsMethodNotFound(t *testing.T) {
	s := newTestServer(t)
	s.Handle("initialize", Interactive, func(context.Context, json.RawMessage) (any, error) { return nil, nil })

	in := strings.NewReader(
		frame(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`) +
			frame(t, `{"jsonrpc":"2.0","id":2,"method":"textDocument/bogus","params":{}}`),
	)
	var out bytes.Buffer
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	frames := readFrames(t, out.Bytes())
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	f := frameForID(t, frames, 2)
	errObj, _ := f["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("frame = %v, want error", f)
	}
	code, ok := errObj["code"].(float64)
	if !ok || int32(code) != methodNotFoundCode {
		t.Fatalf("code = %v, want %d", errObj["code"], methodNotFoundCode)
	}
}

func TestLifecycleRejectsRequestsBeforeInitialize(t *testing.T) {
	s := newTestServer(t)
	s.Handle("initialize", Interactive, func(context.Context, json.RawMessage) (any, error) { return nil, nil })
	s.Handle("textDocument/hover", Interactive, func(context.Context, json.RawMessage) (any, error) { return "hover", nil })

	in := strings.NewReader(frame(t, `{"jsonrpc":"2.0","id":1,"method":"textDocument/hover","params":{}}`))
	var out bytes.Buffer
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	frames := readFrames(t, out.Bytes())
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	errObj, _ := frames[0]["error"].(map[string]any)
	code, ok := errObj["code"].(float64)
	if errObj == nil || !ok || int32(code) != serverNotInitializedCode {
		t.Fatalf("frame = %v, want ServerNotInitialized", frames[0])
	}
}

func TestLifecycleRejectsRequestsAfterShutdown(t *testing.T) {
	s := newTestServer(t)
	s.Handle("initialize", Interactive, func(context.Context, json.RawMessage) (any, error) { return nil, nil })
	s.Handle("shutdown", Interactive, func(context.Context, json.RawMessage) (any, error) { return nil, nil })
	s.Handle("textDocument/hover", Interactive, func(context.Context, json.RawMessage) (any, error) { return "hover", nil })

	in := strings.NewReader(
		frame(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`) +
			frame(t, `{"jsonrpc":"2.0","id":2,"method":"shutdown"}`) +
			frame(t, `{"jsonrpc":"2.0","id":3,"method":"textDocument/hover","params":{}}`),
	)
	var out bytes.Buffer
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	frames := readFrames(t, out.Bytes())
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3: %v", len(frames), frames)
	}
	f := frameForID(t, frames, 3)
	errObj, _ := f["error"].(map[string]any)
	code, ok := errObj["code"].(float64)
	if errObj == nil || !ok || int32(code) != invalidRequestCode {
		t.Fatalf("frame = %v, want InvalidRequest", f)
	}
}

func TestExitAfterShutdownExitsCleanly(t *testing.T) {
	s := newTestServer(t)
	s.Handle("initialize", Interactive, func(context.Context, json.RawMessage) (any, error) { return nil, nil })
	s.Handle("shutdown", Interactive, func(context.Context, json.RawMessage) (any, error) { return nil, nil })

	in := strings.NewReader(
		frame(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`) +
			frame(t, `{"jsonrpc":"2.0","id":2,"method":"shutdown"}`) +
			frame(t, `{"jsonrpc":"2.0","method":"exit"}`),
	)
	err := s.Serve(context.Background(), in, &bytes.Buffer{})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Serve() error = %v, want *ExitError", err)
	}
	if exitErr.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (shutdown preceded exit)", exitErr.Code)
	}
}

func TestExitWithoutShutdownExitsWithCodeOne(t *testing.T) {
	s := newTestServer(t)
	s.Handle("initialize", Interactive, func(context.Context, json.RawMessage) (any, error) { return nil, nil })

	in := strings.NewReader(
		frame(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`) +
			frame(t, `{"jsonrpc":"2.0","method":"exit"}`),
	)
	err := s.Serve(context.Background(), in, &bytes.Buffer{})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Serve() error = %v, want *ExitError", err)
	}
	if exitErr.Code != 1 {
		t.Fatalf("exit code = %d, want 1 (no shutdown before exit)", exitErr.Code)
	}
}

func TestCancelRequestCancelsHandlerContext(t *testing.T) {
	s := newTestServer(t)
	started := make(chan struct{})
	s.Handle("initialize", Interactive, func(context.Context, json.RawMessage) (any, error) { return nil, nil })
	s.Handle("slow", Background, func(ctx context.Context, _ json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})

	in := strings.NewReader(
		frame(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`) +
			frame(t, `{"jsonrpc":"2.0","id":2,"method":"slow","params":{}}`) +
			frame(t, `{"jsonrpc":"2.0","method":"$/cancelRequest","params":{"id":2}}`),
	)
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- s.Serve(context.Background(), in, &out) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never started")
	}

	if err := <-done; err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	frames := readFrames(t, out.Bytes())
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2: %v", len(frames), frames)
	}
	f := frameForID(t, frames, 2)
	errObj, _ := f["error"].(map[string]any)
	code, ok := errObj["code"].(float64)
	if errObj == nil || !ok || int32(code) != requestCancelledCode {
		t.Fatalf("frame = %v, want RequestCancelled", f)
	}
}

func TestCancelRequestForUnknownIDIsNoop(t *testing.T) {
	s := newTestServer(t)
	// No request with id=99 was ever sent; the cancel notification must be
	// silently ignored rather than causing an error or panic.
	in := strings.NewReader(frame(t, `{"jsonrpc":"2.0","method":"$/cancelRequest","params":{"id":99}}`))
	if err := s.Serve(context.Background(), in, &bytes.Buffer{}); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
}

func TestNotificationsForSameURIRunInOrder(t *testing.T) {
	s := newTestServer(t)
	var mu sync.Mutex
	var order []int
	release := make(chan struct{})
	s.Handle("initialize", Interactive, func(context.Context, json.RawMessage) (any, error) { return nil, nil })
	s.HandleNotification("first", func(context.Context, json.RawMessage) error {
		<-release // force the second notification to queue behind this one
		mu.Lock()
		order = append(order, 1)
		mu.Unlock()
		return nil
	})
	s.HandleNotification("second", func(context.Context, json.RawMessage) error {
		mu.Lock()
		order = append(order, 2)
		mu.Unlock()
		return nil
	})

	uri := `{"textDocument":{"uri":"file:///a.go"}}`
	in := strings.NewReader(
		frame(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`) +
			frame(t, fmt.Sprintf(`{"jsonrpc":"2.0","method":"first","params":%s}`, uri)) +
			frame(t, fmt.Sprintf(`{"jsonrpc":"2.0","method":"second","params":%s}`, uri)),
	)
	done := make(chan error, 1)
	go func() { done <- s.Serve(context.Background(), in, &bytes.Buffer{}) }()

	// Give the dispatcher time to enqueue both notifications behind "first"
	// before releasing it, so the test actually exercises queuing rather
	// than accidentally passing due to scheduling luck.
	time.Sleep(50 * time.Millisecond)
	close(release)

	if err := <-done; err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	// Notification handlers run in a goroutine drained from the queue;
	// wait for both to finish recording their order.
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("notifications did not both run, order = %v", order)
		case <-time.After(time.Millisecond):
		}
	}
	if order[0] != 1 || order[1] != 2 {
		t.Fatalf("order = %v, want [1 2]", order)
	}
}

func TestPlainErrorIsReportedAsInternalError(t *testing.T) {
	s := newTestServer(t)
	s.Handle("initialize", Interactive, func(context.Context, json.RawMessage) (any, error) {
		return nil, errors.New("boom")
	})
	in := strings.NewReader(frame(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	var out bytes.Buffer
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	frames := readFrames(t, out.Bytes())
	errObj, _ := frames[0]["error"].(map[string]any)
	code, ok := errObj["code"].(float64)
	if errObj == nil || !ok || int32(code) != internalErrorCode {
		t.Fatalf("frame = %v, want InternalError", frames[0])
	}
	if errObj["message"] != "boom" {
		t.Fatalf("message = %v, want boom", errObj["message"])
	}
}

// TestPanickingRequestHandlerReturnsInternalErrorAndServerKeepsServing
// covers Finding 4: an unrecovered panic in one handler used to crash the
// whole process. A panicking handler must now produce an InternalError
// response for just that request, and the server must keep serving later
// requests on the same connection afterward.
func TestPanickingRequestHandlerReturnsInternalErrorAndServerKeepsServing(t *testing.T) {
	s := newTestServer(t)
	s.Handle("initialize", Interactive, func(context.Context, json.RawMessage) (any, error) { return nil, nil })
	s.Handle("boom", Interactive, func(context.Context, json.RawMessage) (any, error) {
		panic("deliberate handler panic")
	})
	s.Handle("textDocument/hover", Interactive, func(context.Context, json.RawMessage) (any, error) {
		return "still serving", nil
	})

	in := strings.NewReader(
		frame(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`) +
			frame(t, `{"jsonrpc":"2.0","id":2,"method":"boom","params":{}}`) +
			frame(t, `{"jsonrpc":"2.0","id":3,"method":"textDocument/hover","params":{}}`),
	)
	var out bytes.Buffer
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	frames := readFrames(t, out.Bytes())
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3: %v", len(frames), frames)
	}

	f := frameForID(t, frames, 2)
	errObj, _ := f["error"].(map[string]any)
	code, ok := errObj["code"].(float64)
	if errObj == nil || !ok || int32(code) != internalErrorCode {
		t.Fatalf("panicking handler's response = %v, want InternalError", f)
	}
	if msg, _ := errObj["message"].(string); strings.Contains(msg, "deliberate handler panic") {
		t.Fatalf("client-facing message = %q, must not leak the panic value", msg)
	}

	f3 := frameForID(t, frames, 3)
	if f3["result"] != "still serving" {
		t.Fatalf("request after the panic = %v, want the server to keep answering requests", f3)
	}
}

// TestPanickingNotificationHandlerIsSwallowedAndServerKeepsServing covers
// Finding 4's notification half: a panicking notification handler has no
// response to send, so it must be logged and swallowed rather than crashing
// the process or wedging that notification's per-document queue.
func TestPanickingNotificationHandlerIsSwallowedAndServerKeepsServing(t *testing.T) {
	s := newTestServer(t)
	s.Handle("initialize", Interactive, func(context.Context, json.RawMessage) (any, error) { return nil, nil })
	s.HandleNotification("boom", func(context.Context, json.RawMessage) error {
		panic("deliberate notification panic")
	})
	ran := make(chan struct{}, 1)
	s.HandleNotification("after", func(context.Context, json.RawMessage) error {
		ran <- struct{}{}
		return nil
	})

	uri := `{"textDocument":{"uri":"file:///a.go"}}`
	in := strings.NewReader(
		frame(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`) +
			frame(t, fmt.Sprintf(`{"jsonrpc":"2.0","method":"boom","params":%s}`, uri)) +
			frame(t, fmt.Sprintf(`{"jsonrpc":"2.0","method":"after","params":%s}`, uri)),
	)
	var out bytes.Buffer
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("notification queued after the panicking one never ran; the queue is wedged")
	}
}

func TestHandlerReturnedErrorPreservesCode(t *testing.T) {
	s := newTestServer(t)
	s.Handle("initialize", Interactive, func(context.Context, json.RawMessage) (any, error) {
		return nil, NewError(invalidRequestCode, "bad request")
	})
	in := strings.NewReader(frame(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	var out bytes.Buffer
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	frames := readFrames(t, out.Bytes())
	errObj, _ := frames[0]["error"].(map[string]any)
	code, ok := errObj["code"].(float64)
	if errObj == nil || !ok || int32(code) != invalidRequestCode {
		t.Fatalf("frame = %v, want InvalidRequest", frames[0])
	}
}

// syncBuffer is a bytes.Buffer safe for one writer goroutine and one reader
// goroutine coordinating through wait: Write signals wait (non-blocking, so
// a burst of writes before the reader catches up never blocks the writer)
// after every write, and Bytes takes its own lock so a concurrent Write
// never races it.
type syncBuffer struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	wait chan struct{}
}

func newSyncBuffer() *syncBuffer {
	return &syncBuffer{wait: make(chan struct{}, 1)}
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, err := b.buf.Write(p)
	b.mu.Unlock()
	select {
	case b.wait <- struct{}{}:
	default:
	}
	return n, err
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// waitForFrame blocks until out has at least one frame, or t.Fatal on
// timeout, returning that frame's id re-encoded as a json.RawMessage
// suitable for a synthetic message.ID.
func waitForFrame(t *testing.T, out *syncBuffer) json.RawMessage {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-out.wait:
		case <-deadline:
			t.Fatal("no request frame written within 5s")
		}
		frames := readFrames(t, out.Bytes())
		if len(frames) == 1 {
			b, err := json.Marshal(frames[0]["id"])
			if err != nil {
				t.Fatalf("marshal request id: %v", err)
			}
			return b
		}
	}
}

func TestRequestReturnsClientResponse(t *testing.T) {
	s := newTestServer(t)
	out := newSyncBuffer()
	s.conn = newConn(out)

	type outcome struct {
		result json.RawMessage
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := s.Request(context.Background(), "client/registerCapability", map[string]string{"id": "watch-go"})
		done <- outcome{result, err}
	}()

	// Reply as the client would, echoing back the id Request assigned.
	id := waitForFrame(t, out)
	s.dispatchResponse(&message{ID: id, Result: json.RawMessage(`{"ok":true}`)})

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Request() error = %v", got.err)
		}
		if string(got.result) != `{"ok":true}` {
			t.Fatalf("Request() result = %s, want {\"ok\":true}", got.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Request() did not return after its response was dispatched")
	}
}

func TestRequestReturnsClientError(t *testing.T) {
	s := newTestServer(t)
	out := newSyncBuffer()
	s.conn = newConn(out)

	done := make(chan error, 1)
	go func() {
		_, err := s.Request(context.Background(), "client/registerCapability", map[string]string{})
		done <- err
	}()

	id := waitForFrame(t, out)
	s.dispatchResponse(&message{ID: id, Error: &wireError{Code: internalErrorCode, Message: "not supported"}})

	select {
	case err := <-done:
		var rpcErr *Error
		if !errors.As(err, &rpcErr) || rpcErr.Message != "not supported" {
			t.Fatalf("Request() error = %v, want an *Error wrapping %q", err, "not supported")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Request() did not return after its error response was dispatched")
	}
}

func TestRequestReturnsContextErrorOnTimeout(t *testing.T) {
	s := newTestServer(t)
	out := newSyncBuffer()
	s.conn = newConn(out)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := s.Request(ctx, "client/registerCapability", map[string]string{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Request() error = %v, want context.DeadlineExceeded", err)
	}

	// The abandoned pending entry must not still be there, or a
	// since-deregistered client response arriving late would leak: dispatch
	// one and confirm it is silently dropped rather than panicking.
	id := waitForFrame(t, out)
	s.dispatchResponse(&message{ID: id, Result: json.RawMessage(`{}`)})
}

func TestNotifyWritesNotificationFrame(t *testing.T) {
	s := newTestServer(t)
	var out bytes.Buffer
	s.conn = newConn(&out)
	if err := s.Notify("textDocument/publishDiagnostics", map[string]string{"uri": "file:///a.go"}); err != nil {
		t.Fatalf("Notify() error = %v", err)
	}
	frames := readFrames(t, out.Bytes())
	if len(frames) != 1 || frames[0]["method"] != "textDocument/publishDiagnostics" {
		t.Fatalf("frames = %v", frames)
	}
}

// TestContext_BeforeServeReturnsBackground verifies that Context is safe to
// call before Serve has ever run — e.g. a handler exercised directly by a
// unit test, without a real Serve session — falling back to
// context.Background() instead of a nil context that would panic a caller
// like exec.CommandContext.
func TestContext_BeforeServeReturnsBackground(t *testing.T) {
	s := newTestServer(t)
	if got := s.Context(); got == nil || got.Err() != nil {
		t.Fatalf("Context() before Serve = %v, want a non-nil, not-yet-canceled context.Background()", got)
	}
}

// TestContext_CanceledWhenServeReturns verifies the invariant Go-launched
// background work relies on to stop instead of outliving the session: once
// Serve returns (here, on a clean EOF), Context is canceled.
func TestContext_CanceledWhenServeReturns(t *testing.T) {
	s := newTestServer(t)
	pr, pw := io.Pipe()
	var out bytes.Buffer

	done := make(chan error, 1)
	go func() { done <- s.Serve(context.Background(), pr, &out) }()

	if err := pw.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() error = %v, want nil (clean EOF)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve() did not return after EOF")
	}

	if err := s.Context().Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Context().Err() after Serve returned = %v, want context.Canceled", err)
	}
}

// TestGo_TrackedByServeShutdownDrain verifies both of Go's guarantees at
// once: the function it launches observes Context's cancellation once Serve
// returns, and Serve's own defer s.wg.Wait() actually waits for it to
// finish before Serve itself returns to its caller — the property a
// shutdown-time goroutine-leak test relies on, instead of Serve returning
// while Go-launched work is still outstanding. fn is launched from inside a
// registered request handler (as production code always does — see
// internal/server's lifecycle.go/documentsync.go), not directly from the
// test, so this also exercises Go/Context under their real call pattern.
func TestGo_TrackedByServeShutdownDrain(t *testing.T) {
	s := newTestServer(t)
	finished := make(chan struct{})
	s.Handle("initialize", Interactive, func(context.Context, json.RawMessage) (any, error) {
		s.Go(func(ctx context.Context) {
			<-ctx.Done() // blocks until Serve returns and cancels Context()
			close(finished)
		})
		return nil, nil
	})

	pr, pw := io.Pipe()
	var out bytes.Buffer
	go func() {
		_, _ = pw.Write([]byte(frame(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)))
		_ = pw.Close()
	}()

	if err := s.Serve(context.Background(), pr, &out); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	select {
	case <-finished:
	default:
		t.Fatal("Serve() returned before its own wg.Wait() drained the Go-launched goroutine")
	}
}
