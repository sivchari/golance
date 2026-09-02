package server

import (
	"context"
	"fmt"
	"time"
)

// slowRequestThreshold is the minimum total request duration
// instrument-wrapped handlers log at INFO (see (*Server).instrument). Set
// well above golance's own target for a warm navigation query (milliseconds
// to low hundreds of ms), so an ordinary request never logs a line — only
// one slow enough to be worth diagnosing from a user's log file without
// asking them to reproduce it under a profiler.
const slowRequestThreshold = 500 * time.Millisecond

// phaseTimer accumulates named phase durations across a single request's
// call chain, so a slow request's log line can report which phase
// dominated — e.g. "engine.Get" (the workspace package itself was slow to
// check) versus "depcheck.check" (a cold dependency closure check was slow)
// versus "facts" (an on-disk index query was slow) — the exact distinction
// this exists to make in the field. Not safe for concurrent use: one
// instance belongs to exactly one request's own call chain, threaded
// through via context (see withPhaseTimer/phaseTimerFrom). Every method
// tolerates a nil receiver, so a call site that has no phaseTimer in its
// ctx (a handler that predates this instrumentation, or a test calling a
// helper directly with context.Background()) never needs its own nil
// check before calling enter.
type phaseTimer struct {
	phaseStart   time.Time
	phase        string
	longestPhase string
	longestDur   time.Duration

	// unitsVisited/bytesRead/recordsScanned accumulate
	// internal/xref.References' own closure-walk counts (see AddUnit,
	// which implements xref.StatsSink): zero for every handler except
	// handleReferences, which is the only one that installs a phaseTimer
	// as a stats sink (see WithStatsSink). countsSuffix reports these on
	// the slow-request log line only when unitsVisited is nonzero, so
	// every other handler's line is unaffected.
	unitsVisited   int
	bytesRead      int64
	recordsScanned int
}

// enter marks the start of phase name, first crediting whatever phase was
// already open (if any) with the time elapsed since IT started. Call sites
// are expected to call enter for each distinct piece of work a request's
// handler delegates to (an engine.Get, a depcheck check, a facts-index
// query, ...); the timer itself decides which one turns out to have taken
// longest.
func (p *phaseTimer) enter(name string) {
	if p == nil {
		return
	}
	now := time.Now()
	p.creditOpenPhase(now)
	p.phase = name
	p.phaseStart = now
}

// creditOpenPhase records the currently-open phase's elapsed time (up to
// now) against longestDur/longestPhase if it is the longest seen so far.
// No-op if no phase is currently open.
func (p *phaseTimer) creditOpenPhase(now time.Time) {
	if p.phase == "" {
		return
	}
	if d := now.Sub(p.phaseStart); d > p.longestDur {
		p.longestDur = d
		p.longestPhase = p.phase
	}
}

// dominant closes out whatever phase is currently open and returns the
// single longest phase observed over the timer's lifetime — "" and 0 if
// enter was never called (or p is nil).
func (p *phaseTimer) dominant() (name string, dur time.Duration) {
	if p == nil {
		return "", 0
	}
	p.creditOpenPhase(time.Now())
	p.phase = ""
	return p.longestPhase, p.longestDur
}

// EnterPhase implements xref.StatsSink for a References query, so
// internal/xref's own resolve/closureWalk/sortDedup sub-stages can compete
// for "dominant phase" on p's usual timeline (see enter) exactly like
// engine.Get/depcheck.check/facts.* already do -- xref cannot call enter
// directly (phaseTimer is unexported to the server package, and xref
// cannot import server without a cycle), so it calls this instead, threaded
// through ctx via xref.WithStatsSink (see handleReferences).
func (p *phaseTimer) EnterPhase(name string) {
	p.enter(name)
}

// AddUnit implements xref.StatsSink, accumulating one References
// closure-walk unit's Facts size and scanned-record count (see
// unitsVisited's doc).
func (p *phaseTimer) AddUnit(bytesRead int64, recordsScanned int) {
	if p == nil {
		return
	}
	p.unitsVisited++
	p.bytesRead += bytesRead
	p.recordsScanned += recordsScanned
}

// countsSuffix formats p's References closure-walk counts (see AddUnit) as
// a slow-request log line suffix, or "" if AddUnit was never called for
// this request (every handler except References today) -- appended to
// (*Server).instrument's own log line so a slow References query reports
// how many units it actually visited without a second log line.
func (p *phaseTimer) countsSuffix() string {
	if p == nil || p.unitsVisited == 0 {
		return ""
	}
	return fmt.Sprintf(" units_visited=%d bytes_read=%d records_scanned=%d", p.unitsVisited, p.bytesRead, p.recordsScanned)
}

// phaseTimerKey is the context key withPhaseTimer/phaseTimerFrom use.
// Unexported, package-private type so no other package can collide with it.
type phaseTimerKey struct{}

// withPhaseTimer returns a child of ctx carrying a fresh phaseTimer,
// retrievable via phaseTimerFrom by any code the request handler calls
// into, plus that same timer directly for the caller's own use once the
// request completes (see (*Server).instrument).
func withPhaseTimer(ctx context.Context) (context.Context, *phaseTimer) {
	pt := &phaseTimer{}
	return context.WithValue(ctx, phaseTimerKey{}, pt), pt
}

// phaseTimerFrom returns the phaseTimer ctx carries, or nil if none — every
// phaseTimer method tolerates a nil receiver (see its doc), so callers can
// write phaseTimerFrom(ctx).enter("...") unconditionally.
func phaseTimerFrom(ctx context.Context) *phaseTimer {
	pt, _ := ctx.Value(phaseTimerKey{}).(*phaseTimer)
	return pt
}
