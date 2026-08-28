package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
