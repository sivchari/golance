package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"testing"
	"time"
)

// TestBackgroundPool_SlowRequestDoesNotSerializeFastOne is the dispatch-
// level regression test for the "head-of-line blocking" suspect: with the
// default Background pool (see defaultBackgroundWorkers), a slow in-flight
// Background request must not delay an unrelated fast Background request
// dispatched right after it — both dispatchRequest (which spawns a
// goroutine per request rather than blocking the read loop) and the pool
// having more than one slot are what make this true. The fast request is
// only written to the input stream once the slow handler has provably
// started and claimed a pool slot (io.Pipe, staged writes), so this proves
// concurrency rather than racing on goroutine scheduling order. A
// regression to a pool of size 1, or to dispatching requests serially,
// would make the fast response wait for the slow one and this test would
// time out.
func TestBackgroundPool_SlowRequestDoesNotSerializeFastOne(t *testing.T) {
	s := newTestServer(t)
	release := make(chan struct{})
	slowStarted := make(chan struct{})
	s.Handle("initialize", Interactive, func(context.Context, json.RawMessage) (any, error) { return nil, nil })
	s.Handle("slow", Background, func(ctx context.Context, _ json.RawMessage) (any, error) {
		close(slowStarted)
		<-release
		return "slow-done", nil
	})
	s.Handle("fast", Background, func(context.Context, json.RawMessage) (any, error) {
		return "fast-done", nil
	})

	pr, pw := io.Pipe()
	out := newSyncBuffer()
	done := make(chan error, 1)
	go func() { done <- s.Serve(context.Background(), pr, out) }()

	writeFrame(t, pw, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	writeFrame(t, pw, `{"jsonrpc":"2.0","id":2,"method":"slow","params":{}}`)

	select {
	case <-slowStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("slow handler never started")
	}

	// slow now provably holds a Background pool slot; only now send fast.
	writeFrame(t, pw, `{"jsonrpc":"2.0","id":3,"method":"fast","params":{}}`)
	writeFrame(t, pw, `{"jsonrpc":"2.0","method":"exit"}`)
	_ = pw.Close()

	// The fast response must arrive WHILE slow is still blocked on release —
	// proving it was never queued behind slow.
	deadline := time.After(2 * time.Second)
	fastSeen := false
	for !fastSeen {
		select {
		case <-out.wait:
		case <-deadline:
			t.Fatal("fast response did not arrive within 2s while the slow request was still in flight")
		}
		for _, f := range readFrames(t, out.Bytes()) {
			if id, ok := f["id"].(float64); ok && id == 3 {
				fastSeen = true
			}
		}
	}

	close(release)
	<-done // exit notification already queued; Serve returns *ExitError, ignored here
}

// TestBackgroundPool_BoundedSizeSerializesExcessRequests documents the pool
// mechanism itself, as a control for the test above: with the Background
// pool explicitly bounded to 1 (WithBackgroundWorkers(1)), a second
// Background request genuinely does wait for the first to release its slot
// — proving the "fast does not wait for slow" result above comes from
// having enough concurrency, not from the pool bound not actually working.
// As above, fast is only sent once slow has provably claimed the pool's
// sole slot, so which request's goroutine happens to win the race to
// acquire it is not left to chance.
func TestBackgroundPool_BoundedSizeSerializesExcessRequests(t *testing.T) {
	s := NewServer(WithLogger(log.New(&testWriter{t}, "", 0)), WithBackgroundWorkers(1))
	release := make(chan struct{})
	slowStarted := make(chan struct{})
	s.Handle("initialize", Interactive, func(context.Context, json.RawMessage) (any, error) { return nil, nil })
	s.Handle("slow", Background, func(ctx context.Context, _ json.RawMessage) (any, error) {
		close(slowStarted)
		<-release
		return "slow-done", nil
	})
	fastRan := make(chan struct{})
	s.Handle("fast", Background, func(context.Context, json.RawMessage) (any, error) {
		close(fastRan)
		return "fast-done", nil
	})

	pr, pw := io.Pipe()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- s.Serve(context.Background(), pr, &out) }()

	writeFrame(t, pw, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	writeFrame(t, pw, `{"jsonrpc":"2.0","id":2,"method":"slow","params":{}}`)

	select {
	case <-slowStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("slow handler never started")
	}

	writeFrame(t, pw, `{"jsonrpc":"2.0","id":3,"method":"fast","params":{}}`)
	writeFrame(t, pw, `{"jsonrpc":"2.0","method":"exit"}`)
	_ = pw.Close()

	select {
	case <-fastRan:
		t.Fatal("fast handler ran before slow released its slot, with Background bounded to 1")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case <-fastRan:
	case <-time.After(5 * time.Second):
		t.Fatal("fast handler never ran after slow released its slot")
	}
	<-done
}

// writeFrame writes body as one Content-Length-framed message to w, failing
// t on error.
func writeFrame(t *testing.T, w io.Writer, body string) {
	t.Helper()
	if _, err := io.WriteString(w, frame(t, body)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}
