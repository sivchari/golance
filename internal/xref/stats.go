package xref

import "context"

// StatsSink receives References' own per-request instrumentation: which
// resolve/closureWalk/sortDedup stage is currently running, and how much
// the closure walk actually read. This exists so a caller like
// internal/server can report it on its own slow-request log line (see
// internal/server/latency.go's phaseTimer) without xref depending on the
// server package -- server already imports xref, so the reverse dependency
// isn't available, and phaseTimer's own type is unexported besides.
// internal/server's phaseTimer implements this interface and installs
// itself via WithStatsSink before calling Resolver.References.
type StatsSink interface {
	// EnterPhase marks the start of one of References' own sub-stages
	// ("resolve", "closureWalk", or "sortDedup"), crediting whatever
	// stage was previously open with its own elapsed time first --
	// mirrors internal/server's own phaseTimer.enter.
	EnterPhase(name string)
	// AddUnit records one closure-walk unit's Facts size and how many
	// reference records in it were scanned (see locationsForAll).
	AddUnit(bytesRead int64, recordsScanned int)
}

// statsSinkKey is the context key WithStatsSink/statsSinkFromContext use.
type statsSinkKey struct{}

// WithStatsSink returns a child of ctx that References reports its
// resolve/closureWalk/sortDedup stages and closure-walk counts to via
// sink, for the duration of one query.
func WithStatsSink(ctx context.Context, sink StatsSink) context.Context {
	return context.WithValue(ctx, statsSinkKey{}, sink)
}

// statsSinkFromContext returns ctx's installed StatsSink, or nil if none —
// enterPhase/addUnit tolerate a nil result, so call sites need no separate
// nil check.
func statsSinkFromContext(ctx context.Context) StatsSink {
	sink, _ := ctx.Value(statsSinkKey{}).(StatsSink)
	return sink
}

// enterPhase reports name to ctx's installed StatsSink, if any. No-op
// (a context.Value lookup and nothing else) when no caller installed one,
// which is every call site except internal/server's handleReferences.
func enterPhase(ctx context.Context, name string) {
	if sink := statsSinkFromContext(ctx); sink != nil {
		sink.EnterPhase(name)
	}
}

// addUnit reports one closure-walk unit's size and scanned-record count to
// ctx's installed StatsSink, if any.
func addUnit(ctx context.Context, bytesRead int64, recordsScanned int) {
	if sink := statsSinkFromContext(ctx); sink != nil {
		sink.AddUnit(bytesRead, recordsScanned)
	}
}
