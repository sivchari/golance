package xref

import (
	"context"
	"testing"
)

// fakeStatsSink records every EnterPhase/AddUnit call it receives, for
// asserting References' own instrumentation calls (see stats.go) without
// depending on internal/server's phaseTimer.
type fakeStatsSink struct {
	phases []string
	units  []fakeUnitCall
}

type fakeUnitCall struct {
	bytesRead      int64
	recordsScanned int
}

func (f *fakeStatsSink) EnterPhase(name string) { f.phases = append(f.phases, name) }

func (f *fakeStatsSink) AddUnit(bytesRead int64, recordsScanned int) {
	f.units = append(f.units, fakeUnitCall{bytesRead: bytesRead, recordsScanned: recordsScanned})
}

// TestReferences_ReportsStatsToInstalledSink verifies References reports
// its resolve/closureWalk/sortDedup stages, in that order, plus a non-empty
// AddUnit call per closure unit it visits, to a StatsSink installed via
// WithStatsSink — the hook internal/server's handleReferences uses to
// extend its slow-request log line (see internal/server/latency.go's
// phaseTimer.countsSuffix).
func TestReferences_ReportsStatsToInstalledSink(t *testing.T) {
	r, snap := newTestResolver(t)
	implFile := goFile(t, snap, pkgImpl, "impl.go")
	line, col := identOccurrence(t, implFile, "Person")

	sink := &fakeStatsSink{}
	ctx := WithStatsSink(context.Background(), sink)

	if _, err := r.References(ctx, implFile, line, col, true); err != nil {
		t.Fatalf("References: %v", err)
	}

	wantPhases := []string{"resolve", "closureWalk", "sortDedup"}
	if len(sink.phases) != len(wantPhases) {
		t.Fatalf("phases = %v, want %v", sink.phases, wantPhases)
	}
	for i, want := range wantPhases {
		if sink.phases[i] != want {
			t.Errorf("phases[%d] = %q, want %q", i, sink.phases[i], want)
		}
	}

	if len(sink.units) == 0 {
		t.Fatal("AddUnit was never called, want at least one closure unit visited")
	}
	for _, u := range sink.units {
		if u.bytesRead <= 0 {
			t.Errorf("AddUnit bytesRead = %d, want > 0", u.bytesRead)
		}
	}
}

// TestReferences_NoStatsSinkIsNoOp verifies References works identically
// with no StatsSink installed (the overwhelmingly common case: only
// internal/server's handleReferences installs one) — enterPhase/addUnit
// must never panic or otherwise misbehave on a plain context.Background().
func TestReferences_NoStatsSinkIsNoOp(t *testing.T) {
	r, snap := newTestResolver(t)
	implFile := goFile(t, snap, pkgImpl, "impl.go")
	line, col := identOccurrence(t, implFile, "Person")

	if _, err := r.References(context.Background(), implFile, line, col, true); err != nil {
		t.Fatalf("References: %v", err)
	}
}
