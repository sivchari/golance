package rpc

import "sync"

// notifQueue runs pushed funcs one at a time, in the order they were pushed.
// It holds no goroutine while idle: push starts a drain goroutine only when
// the queue was empty, and that goroutine exits once it drains the queue.
type notifQueue struct {
	mu      sync.Mutex
	pending []func()
	running bool
}

func (q *notifQueue) push(fn func()) {
	q.mu.Lock()
	q.pending = append(q.pending, fn)
	if q.running {
		q.mu.Unlock()
		return
	}
	q.running = true
	q.mu.Unlock()
	go q.drain()
}

func (q *notifQueue) drain() {
	for {
		q.mu.Lock()
		if len(q.pending) == 0 {
			q.running = false
			q.mu.Unlock()
			return
		}
		fn := q.pending[0]
		q.pending = q.pending[1:]
		q.mu.Unlock()
		fn()
	}
}
