package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// blockingHandler returns a RequestHandler that blocks until either ctx is
// done (returning ctx.Err()) or release is closed (returning a fixed
// success result), for driving dispatchRequest's timeout/cancellation paths
// under testing/synctest's fake clock. release may be nil, in which case the
// handler can only ever return via ctx.Done().
func blockingHandler(release <-chan struct{}) RequestHandler {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return map[string]string{"ok": "true"}, nil
		}
	}
}

// TestDispatchRequest_BackgroundTimesOut verifies a Background-priority
// request whose handler never returns on its own is cut off by
// Server.requestTimeout: the client sees the same requestCancelled response
// a $/cancelRequest produces, and the server logs that the cause was its own
// deadline (not a client cancel). Drives dispatchRequest directly (rather
// than through Serve's own read loop over a pipe) and synchronizes with
// synctest.Wait() instead of racing a wall-clock sleep against the
// response, so nothing is left durably blocked once the bubble's root
// goroutine (this test function) returns -- see testing/synctest's own
// "Time stops advancing when the root goroutine of the bubble exits" and
// its Context.WithTimeout example, which this test's structure mirrors.
func TestDispatchRequest_BackgroundTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logBuf bytes.Buffer
		var out bytes.Buffer
		s := NewServer(WithLogger(log.New(&logBuf, "", 0)))
		s.conn = newConn(&out)
		s.requestTimeout = 10 * time.Millisecond
		s.state.Store(int32(stateInitialized))
		s.Handle("workspace/references", Background, blockingHandler(nil))

		s.dispatchRequest(context.Background(), &message{
			JSONRPC: jsonrpcVersion, ID: json.RawMessage(`1`),
			Method: "workspace/references", Params: json.RawMessage(`{}`),
		})
		// Advance past the deadline (synctest.Wait() alone only waits for
		// the bubble to settle; it never itself advances time -- see the
		// package doc's Context.WithTimeout example, which this mirrors),
		// then let the now-canceled handler's response finish writing.
		time.Sleep(s.requestTimeout + time.Millisecond)
		synctest.Wait()

		frames := readFrames(t, out.Bytes())
		if len(frames) != 1 {
			t.Fatalf("got %d response frames, want 1: %v", len(frames), frames)
		}
		errObj, ok := frames[0]["error"].(map[string]any)
		if !ok {
			t.Fatalf("response = %v, want an error response", frames[0])
		}
		code, ok := errObj["code"].(float64)
		if !ok {
			t.Fatalf("error code = %v, want a JSON number", errObj["code"])
		}
		if int32(code) != requestCancelledCode {
			t.Errorf("error code = %v, want requestCancelledCode (%d)", code, requestCancelledCode)
		}
		if !strings.Contains(logBuf.String(), "timeout") {
			t.Errorf("log output = %q, want it to mention the server-side timeout", logBuf.String())
		}
	})
}

// TestDispatchRequest_InteractiveNeverTimesOut verifies Interactive-priority
// requests are never given a deadline by Server.requestTimeout: a handler
// that would have been cut off as Background keeps running well past that
// same duration, and still answers with its own result once released.
func TestDispatchRequest_InteractiveNeverTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var out bytes.Buffer
		s := newTestServer(t)
		s.conn = newConn(&out)
		s.requestTimeout = 10 * time.Millisecond
		s.state.Store(int32(stateInitialized))
		release := make(chan struct{})
		s.Handle("textDocument/hover", Interactive, blockingHandler(release))

		s.dispatchRequest(context.Background(), &message{
			JSONRPC: jsonrpcVersion, ID: json.RawMessage(`1`),
			Method: "textDocument/hover", Params: json.RawMessage(`{}`),
		})

		// Advance well past the Background-equivalent timeout: nothing
		// fires since Interactive requests are never given a deadline, so
		// the handler is still blocked on release when this returns.
		time.Sleep(1 * time.Second)
		if got := out.Bytes(); len(got) != 0 {
			t.Fatalf("got a response before the handler was released, want none: %s", got)
		}

		close(release)
		synctest.Wait()
		frames := readFrames(t, out.Bytes())
		if len(frames) != 1 {
			t.Fatalf("got %d response frames, want 1: %v", len(frames), frames)
		}
		result, ok := frames[0]["result"].(map[string]any)
		if !ok || result["ok"] != "true" {
			t.Fatalf("response = %v, want the handler's own success result", frames[0])
		}
	})
}

// TestDispatchRequest_ClientCancelDoesNotLogTimeout verifies the new
// deadline-only log line in dispatchRequest's canceled/deadline-exceeded
// branch does not fire for an ordinary client-initiated $/cancelRequest,
// only for Server.requestTimeout's own deadline.
func TestDispatchRequest_ClientCancelDoesNotLogTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logBuf bytes.Buffer
		var out bytes.Buffer
		s := NewServer(WithLogger(log.New(&logBuf, "", 0)))
		s.conn = newConn(&out)
		s.requestTimeout = 10 * time.Second // long enough that the explicit cancel below fires first
		s.state.Store(int32(stateInitialized))
		s.Handle("workspace/references", Background, blockingHandler(nil))

		s.dispatchRequest(context.Background(), &message{
			JSONRPC: jsonrpcVersion, ID: json.RawMessage(`1`),
			Method: "workspace/references", Params: json.RawMessage(`{}`),
		})
		s.handleCancelRequest(json.RawMessage(`{"id":1}`))
		synctest.Wait()

		frames := readFrames(t, out.Bytes())
		if len(frames) != 1 {
			t.Fatalf("got %d response frames, want 1: %v", len(frames), frames)
		}
		errObj, ok := frames[0]["error"].(map[string]any)
		if !ok {
			t.Fatalf("response = %v, want an error response", frames[0])
		}
		code, ok := errObj["code"].(float64)
		if !ok {
			t.Fatalf("error code = %v, want a JSON number", errObj["code"])
		}
		if int32(code) != requestCancelledCode {
			t.Errorf("error code = %v, want requestCancelledCode (%d)", code, requestCancelledCode)
		}
		if strings.Contains(logBuf.String(), "timeout") {
			t.Errorf("log output = %q, want no timeout line for an explicit client cancel", logBuf.String())
		}
	})
}
