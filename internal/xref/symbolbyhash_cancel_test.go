package xref

import (
	"context"
	"errors"
	"testing"
)

// TestSymbolByHash_CanceledContextDistinctFromNotFound pins the M3 fix:
// symbolByHash used to collapse ctx being canceled (or timing out) mid-read
// into the exact same "ok=false" outcome as idHash simply not existing,
// which a candidate-resolution loop (implementingTypes and its siblings)
// could then quietly `continue` past, coming back with what looked like a
// complete-but-smaller result instead of the aborted query it actually was.
// symbolByHash must now let a caller tell the two apart with errors.Is.
func TestSymbolByHash_CanceledContextDistinctFromNotFound(t *testing.T) {
	r, snap := newTestResolver(t)
	implFile := goFile(t, snap, pkgImpl, "impl.go")
	line, col := identOccurrence(t, implFile, "Person")
	l, c, err := toUint32Pos(line, col)
	if err != nil {
		t.Fatalf("toUint32Pos: %v", err)
	}

	target, err := r.resolveAt(context.Background(), implFile, l, c)
	if err != nil {
		t.Fatalf("resolveAt(Person): %v", err)
	}

	// A healthy lookup succeeds and returns the expected name.
	name, _, _, err := r.symbolByHash(context.Background(), target.PkgHash, target.IDHash)
	if err != nil {
		t.Fatalf("symbolByHash(healthy) = %v, want nil error", err)
	}
	if name != "Person" {
		t.Errorf("symbolByHash(healthy) name = %q, want %q", name, "Person")
	}

	// An ordinary miss (idHash naming nothing) reports errSymbolNotFound,
	// never anything ctx-shaped, and the zero value otherwise.
	name, _, _, err = r.symbolByHash(context.Background(), target.PkgHash, target.IDHash^1)
	if !errors.Is(err, errSymbolNotFound) {
		t.Errorf("symbolByHash(unknown idHash) = %v, want errSymbolNotFound", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("symbolByHash(unknown idHash) = %v, must not look like a cancellation", err)
	}
	if name != "" {
		t.Errorf("symbolByHash(unknown idHash) name = %q, want empty on error", name)
	}

	// The identical, otherwise-healthy lookup against an already-canceled
	// ctx must report the cancellation itself -- not errSymbolNotFound --
	// so a caller can distinguish "genuinely nothing here" from "the query
	// was interrupted" instead of conflating the two.
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	name, _, _, err = r.symbolByHash(canceledCtx, target.PkgHash, target.IDHash)
	if err == nil {
		t.Fatal("symbolByHash(canceled ctx) = nil error, want context.Canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("symbolByHash(canceled ctx) = %v, want context.Canceled", err)
	}
	if errors.Is(err, errSymbolNotFound) {
		t.Errorf("symbolByHash(canceled ctx) = %v, must not be reported as an ordinary miss", err)
	}
	if name != "" {
		t.Errorf("symbolByHash(canceled ctx) name = %q, want empty on error", name)
	}
}

// TestLocationsOfSymbols_CanceledContextPropagatesError pins the same fix
// at one of its candidate-resolution call sites: locationsOfSymbols used to
// let a symbolByHash failure of ANY kind -- including the caller's own ctx
// having been canceled -- silently drop that symbol from the result, so a
// canceled Implementation()/References() query on a method could come back
// with a shorter-than-expected but still error-free location list. It must
// now abort and report the cancellation instead.
func TestLocationsOfSymbols_CanceledContextPropagatesError(t *testing.T) {
	r, snap := newTestResolver(t)
	implFile := goFile(t, snap, pkgImpl, "impl.go")
	line, col := identOccurrence(t, implFile, "Person")
	l, c, err := toUint32Pos(line, col)
	if err != nil {
		t.Fatalf("toUint32Pos: %v", err)
	}

	target, err := r.resolveAt(context.Background(), implFile, l, c)
	if err != nil {
		t.Fatalf("resolveAt(Person): %v", err)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	locs, err := r.locationsOfSymbols(canceledCtx, []resolvedSymbol{target})
	if err == nil {
		t.Fatalf("locationsOfSymbols(canceled ctx) = %+v, nil error; want a propagated cancellation instead of a silently short result", locs)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("locationsOfSymbols(canceled ctx) error = %v, want context.Canceled", err)
	}
	if locs != nil {
		t.Errorf("locationsOfSymbols(canceled ctx) locs = %+v, want nil alongside the error", locs)
	}
}
