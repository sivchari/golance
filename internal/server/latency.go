package server

import (
	"context"
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
