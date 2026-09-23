package rpc

import (
	"context"
	"encoding/json"
	"io"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// TestDispatchRequest_WaitsForSameDocumentNotificationQueuedBeforeIt is the
// dispatch-level regression test for the "didOpen immediately followed by a
// completion request for the same file returns stale/empty results" class
// of bug: dispatchRequest and dispatchNotification each spawn their own
// goroutine (see dispatchRequest's pool.run and dispatchNotification's
// notifQueue.push), with no ordering guarantee between a notification's
// handler actually running and a request dispatched right after it on the
// wire, even though a well-behaved client relies on the server observing
// its own notifications (didOpen/didChange) before answering a request for
// the same document that followed them. Two requests/notifications for
// DIFFERENT documents are still free to run concurrently — this only
// orders same-document notification-then-request pairs.
func TestDispatchRequest_WaitsForSameDocumentNotificationQueuedBeforeIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestServer(t)
		release := make(chan struct{})
		notifStarted := make(chan struct{})
		var notifDone atomic.Bool
		s.Handle("initialize", Interactive, func(context.Context, json.RawMessage) (any, error) { return nil, nil })
		s.HandleNotification("textDocument/didOpen", func(context.Context, json.RawMessage) error {
			close(notifStarted)
			<-release
			notifDone.Store(true)
			return nil
		})
		s.Handle("textDocument/completion", Interactive, func(context.Context, json.RawMessage) (any, error) {
			return map[string]bool{"notifDone": notifDone.Load()}, nil
		})

		pr, pw := io.Pipe()
		out := newSyncBuffer()
		done := make(chan error, 1)
		go func() { done <- s.Serve(context.Background(), pr, out) }()

		writeFrame(t, pw, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
		writeFrame(t, pw, `{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///a.go"}}}`)
		writeFrame(t, pw, `{"jsonrpc":"2.0","id":2,"method":"textDocument/completion","params":{"textDocument":{"uri":"file:///a.go"}}}`)

		synctest.Wait()

		select {
		case <-notifStarted:
		default:
			t.Fatal("didOpen notification handler never started")
		}
		for _, f := range readFrames(t, out.Bytes()) {
			if id, ok := f["id"].(float64); ok && id == 2 {
				t.Errorf("completion (id=2) already answered while its preceding same-document didOpen is still in flight: %v", f)
			}
		}

		close(release)
		synctest.Wait()

		frames := readFrames(t, out.Bytes())
		resp := frameForID(t, frames, 2)
		result, _ := resp["result"].(map[string]any)
		if notifDone, _ := result["notifDone"].(bool); !notifDone {
			t.Errorf("completion result = %v, want notifDone=true (the preceding didOpen must have finished first)", resp)
		}

		writeFrame(t, pw, `{"jsonrpc":"2.0","method":"exit"}`)
		_ = pw.Close()
		<-done
	})
}
