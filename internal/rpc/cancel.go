package rpc

import (
	"context"
	"sync"
)

// cancelRegistry maps in-flight request IDs to their cancel funcs so
// $/cancelRequest can cancel them by ID. A cancel for an ID that has already
// been unregistered (answered) or was never registered (unknown) is a no-op.
type cancelRegistry struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func (r *cancelRegistry) register(id string, cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancels == nil {
		r.cancels = make(map[string]context.CancelFunc)
	}
	r.cancels[id] = cancel
}

func (r *cancelRegistry) unregister(id string) {
	r.mu.Lock()
	delete(r.cancels, id)
	r.mu.Unlock()
}

func (r *cancelRegistry) cancel(id string) {
	r.mu.Lock()
	cancel := r.cancels[id]
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
