package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/sivchari/golance/internal/rpc"
)

// TestPhaseTimer_DominantPhase verifies dominant reports the single longest
// phase across several enter calls, not the last one entered or their sum —
// the distinction a slow-request log line needs to point at the right
// subsystem (see phaseTimer's doc).
func TestPhaseTimer_DominantPhase(t *testing.T) {
	pt := &phaseTimer{}
	pt.enter("facts")
	time.Sleep(2 * time.Millisecond)
	pt.enter("engine.Get")
	time.Sleep(10 * time.Millisecond)
	pt.enter("depcheck.check")
	time.Sleep(2 * time.Millisecond)

	name, dur := pt.dominant()
	if name != "engine.Get" {
		t.Errorf("dominant() phase = %q, want %q", name, "engine.Get")
	}
	if dur < 8*time.Millisecond {
		t.Errorf("dominant() duration = %s, want at least ~10ms", dur)
	}
}

// TestPhaseTimer_NilSafe verifies every phaseTimer method tolerates a nil
// receiver without panicking — the property phaseTimerFrom's callers rely
// on to skip a nil check at every call site.
func TestPhaseTimer_NilSafe(t *testing.T) {
	var pt *phaseTimer
	pt.enter("engine.Get")
	name, dur := pt.dominant()
	if name != "" || dur != 0 {
		t.Errorf("dominant() on a nil *phaseTimer = (%q, %s), want (\"\", 0)", name, dur)
	}
}

// TestPhaseTimerFrom_RoundTrip verifies withPhaseTimer/phaseTimerFrom carry
// the identical *phaseTimer instance through a context.
func TestPhaseTimerFrom_RoundTrip(t *testing.T) {
	ctx, pt := withPhaseTimer(context.Background())
	if got := phaseTimerFrom(ctx); got != pt {
		t.Errorf("phaseTimerFrom returned a different *phaseTimer than withPhaseTimer produced")
	}
	if got := phaseTimerFrom(context.Background()); got != nil {
		t.Errorf("phaseTimerFrom(context.Background()) = %v, want nil", got)
	}
}

// TestPhaseTimer_AddUnitAndCountsSuffix verifies AddUnit accumulates across
// several calls and countsSuffix formats them, versus reporting "" when
// AddUnit was never called — the distinction that keeps every non-References
// handler's slow-request line unaffected (see phaseTimer.countsSuffix).
func TestPhaseTimer_AddUnitAndCountsSuffix(t *testing.T) {
	pt := &phaseTimer{}
	if got := pt.countsSuffix(); got != "" {
		t.Errorf("countsSuffix() before any AddUnit = %q, want \"\"", got)
	}

	pt.AddUnit(100, 5)
	pt.AddUnit(50, 3)

	want := " units_visited=2 bytes_read=150 records_scanned=8"
	if got := pt.countsSuffix(); got != want {
		t.Errorf("countsSuffix() = %q, want %q", got, want)
	}
}

// TestPhaseTimer_AddUnitNilSafe verifies AddUnit and countsSuffix tolerate
// a nil receiver, the same property TestPhaseTimer_NilSafe pins for enter/
// dominant — a call site (e.g. xref's addUnit helper) never needs its own
// nil check.
func TestPhaseTimer_AddUnitNilSafe(t *testing.T) {
	var pt *phaseTimer
	pt.AddUnit(10, 1)
	if got := pt.countsSuffix(); got != "" {
		t.Errorf("countsSuffix() on a nil *phaseTimer = %q, want \"\"", got)
	}
}

// TestPhaseTimer_EnterPhaseImplementsStatsSink verifies EnterPhase (xref.
// StatsSink's method) behaves exactly like phaseTimer's own enter, so xref's
// resolve/closureWalk/sortDedup stages compete for "dominant phase" on the
// SAME timeline every other instrumented handler's phases do.
func TestPhaseTimer_EnterPhaseImplementsStatsSink(t *testing.T) {
	pt := &phaseTimer{}
	pt.EnterPhase("resolve")
	time.Sleep(2 * time.Millisecond)
	pt.EnterPhase("closureWalk")
	time.Sleep(10 * time.Millisecond)
	pt.EnterPhase("sortDedup")

	name, dur := pt.dominant()
	if name != "closureWalk" {
		t.Errorf("dominant() phase = %q, want %q", name, "closureWalk")
	}
	if dur < 8*time.Millisecond {
		t.Errorf("dominant() duration = %s, want at least ~10ms", dur)
	}
}

// TestServerInstrument_LogsSlowRequestWithDominantPhase verifies instrument
// logs a slow (>= slowRequestThreshold) request's method, duration, and
// dominant phase, when the wrapped handler entered one via its ctx.
func TestServerInstrument_LogsSlowRequestWithDominantPhase(t *testing.T) {
	var buf bytes.Buffer
	s := New(rpc.NewServer(), Options{Logger: log.New(&buf, "", 0)})

	h := s.instrument("textDocument/definition", func(ctx context.Context, _ json.RawMessage) (any, error) {
		phaseTimerFrom(ctx).enter("depcheck.check")
		time.Sleep(slowRequestThreshold + 10*time.Millisecond)
		return "result", nil
	})

	result, err := h(context.Background(), nil)
	if err != nil {
		t.Fatalf("instrument-wrapped handler returned error: %v", err)
	}
	if result != "result" {
		t.Errorf("instrument-wrapped handler result = %v, want %q", result, "result")
	}

	logged := buf.String()
	if !strings.Contains(logged, "method=textDocument/definition") {
		t.Errorf("log line %q does not mention the method", logged)
	}
	if !strings.Contains(logged, "dominant_phase=depcheck.check") {
		t.Errorf("log line %q does not mention the dominant phase", logged)
	}
}

// TestServerInstrument_LogsReferencesCounts verifies instrument's slow-
// request log line includes the units_visited/bytes_read/records_scanned
// suffix (see phaseTimer.countsSuffix) when the wrapped handler reports
// AddUnit calls through its ctx's phaseTimer — the shape handleReferences
// installs itself as an xref.StatsSink for (see handlers_xref.go).
func TestServerInstrument_LogsReferencesCounts(t *testing.T) {
	var buf bytes.Buffer
	s := New(rpc.NewServer(), Options{Logger: log.New(&buf, "", 0)})

	h := s.instrument("textDocument/references", func(ctx context.Context, _ json.RawMessage) (any, error) {
		pt := phaseTimerFrom(ctx)
		pt.enter("closureWalk")
		pt.AddUnit(1024, 40)
		pt.AddUnit(512, 20)
		time.Sleep(slowRequestThreshold + 10*time.Millisecond)
		return "result", nil
	})

	if _, err := h(context.Background(), nil); err != nil {
		t.Fatalf("instrument-wrapped handler returned error: %v", err)
	}

	logged := buf.String()
	if !strings.Contains(logged, "units_visited=2 bytes_read=1536 records_scanned=60") {
		t.Errorf("log line %q does not contain the expected counts suffix", logged)
	}
}

// TestServerInstrument_FastRequestNeverLogs verifies instrument stays
// silent for a request that finishes under slowRequestThreshold, so a
// user's ordinary session never accumulates log noise.
func TestServerInstrument_FastRequestNeverLogs(t *testing.T) {
	var buf bytes.Buffer
	s := New(rpc.NewServer(), Options{Logger: log.New(&buf, "", 0)})

	h := s.instrument("textDocument/hover", func(ctx context.Context, _ json.RawMessage) (any, error) {
		phaseTimerFrom(ctx).enter("engine.Get")
		return nil, nil
	})
	if _, err := h(context.Background(), nil); err != nil {
		t.Fatalf("instrument-wrapped handler returned error: %v", err)
	}

	if got := buf.String(); got != "" {
		t.Errorf("instrument logged for a fast request: %q", got)
	}
}
