package rpc

// Priority selects which worker pool a registered request handler runs on,
// so an expensive background query (e.g. workspace-wide cross-references)
// cannot starve latency-sensitive interactive requests (e.g. completion).
type Priority uint8

const (
	// Interactive is for latency-sensitive requests such as completion,
	// hover, and diagnostics-adjacent queries.
	Interactive Priority = iota
	// Background is for expensive, less latency-sensitive requests such as
	// workspace-wide cross-reference queries.
	Background
)

// pool bounds how many handlers run concurrently. A nil sem means
// unbounded: every run spawns a goroutine immediately. A non-nil sem still
// spawns a goroutine immediately but blocks it on acquiring a token, which
// bounds concurrent execution without a separate queue — acceptable at LSP
// request volumes.
type pool struct {
	sem chan struct{}
}

func newPool(size int) *pool {
	if size <= 0 {
		return &pool{}
	}
	return &pool{sem: make(chan struct{}, size)}
}

func (p *pool) run(fn func()) {
	if p.sem == nil {
		go fn()
		return
	}
	go func() {
		p.sem <- struct{}{}
		defer func() { <-p.sem }()
		fn()
	}()
}
