package xref

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
)

// TestResolveNamed_DecodeFailureLoggedOnce pins the field-report hardening
// in ReadExport/logDecodeFailureOnce: a package whose export data cannot
// be decoded fails the SAME way on every later query for as long as it
// stays unreindexed (see typecheck.ReadExport's doc for why r.cache
// serves the identical cached error instead of repeating the decode), and
// that must surface in the diagnostics log exactly once per package, not
// once per query -- otherwise a hot query path hitting the same broken
// package repeatedly would flood the log the same way the uncached decode
// itself used to repeat the ~1s cost on every call.
func TestResolveNamed_DecodeFailureLoggedOnce(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/decodefaillog\n\ngo 1.23\n")
	writeTestFile(t, dir, "iface/iface.go", `package iface

type Repo interface {
	Save() error
}
`)
	writeTestFile(t, dir, "broken/broken.go", `package broken

type Broken[T any] struct{}

func (b Broken[T]) Save() error { return nil }
`)
	writeTestFile(t, dir, "healthy/healthy.go", `package healthy

type Healthy struct{}

func (h Healthy) Save() error { return nil }
`)

	r, snap, db, cas := newResolverAndStoreForDir(t, dir)
	corruptExportData(t, db, cas, "example.com/decodefaillog/broken")

	var logBuf bytes.Buffer
	r.SetLogger(log.New(&logBuf, "", 0))

	ifaceFile := goFile(t, snap, "example.com/decodefaillog/iface", "iface.go")
	line, col := identOccurrence(t, ifaceFile, "Repo")

	for i := 0; i < 3; i++ {
		if _, err := r.Implementation(context.Background(), ifaceFile, line, col); err != nil {
			t.Fatalf("Implementation(Repo) call %d: %v", i, err)
		}
	}

	logged := logBuf.String()
	// Count LOG LINES, not substring occurrences: the wrapped error text
	// itself mentions the package path twice within a single
	// logDecodeFailureOnce call (once from ReadExport's own "decode export
	// data for %s" wrapper, once from gcimporter's inner "for path %s"), so
	// counting substrings would overcount even a single, correctly-deduped
	// log line.
	lines := strings.Split(strings.TrimRight(logged, "\n"), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Errorf("log has %d line(s) across 3 queries, want exactly 1 (deduped): log=%q", len(lines), logged)
	}
	if got := r.cache.FailedLen(); got != 1 {
		t.Errorf("r.cache.FailedLen() = %d, want 1 (one negatively-cached package)", got)
	}
}

// TestResolveNamed_DecodeFailureLogResetsAfterInvalidate confirms
// Invalidate resets logDecodeFailureOnce's dedup state alongside
// r.cache's own negative-cache entry (see Invalidate's doc): a package
// that fails to decode, gets reindexed, and fails again must log again,
// not stay silent from the earlier dedup.
func TestResolveNamed_DecodeFailureLogResetsAfterInvalidate(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/decodefailreset\n\ngo 1.23\n")
	writeTestFile(t, dir, "iface/iface.go", `package iface

type Repo interface {
	Save() error
}
`)
	writeTestFile(t, dir, "broken/broken.go", `package broken

type Broken[T any] struct{}

func (b Broken[T]) Save() error { return nil }
`)

	r, snap, db, cas := newResolverAndStoreForDir(t, dir)
	corruptExportData(t, db, cas, "example.com/decodefailreset/broken")

	var logBuf bytes.Buffer
	r.SetLogger(log.New(&logBuf, "", 0))

	ifaceFile := goFile(t, snap, "example.com/decodefailreset/iface", "iface.go")
	line, col := identOccurrence(t, ifaceFile, "Repo")

	if _, err := r.Implementation(context.Background(), ifaceFile, line, col); err != nil {
		t.Fatalf("Implementation(Repo) first call: %v", err)
	}
	if got := countDecodeFailureLogLines(logBuf.String()); got != 1 {
		t.Fatalf("logDecodeFailureOnce lines after first call = %d, want 1: log=%q", got, logBuf.String())
	}

	r.Invalidate([]string{"example.com/decodefailreset/broken"})
	if got := r.cache.FailedLen(); got != 0 {
		t.Fatalf("r.cache.FailedLen() after Invalidate = %d, want 0", got)
	}

	if _, err := r.Implementation(context.Background(), ifaceFile, line, col); err != nil {
		t.Fatalf("Implementation(Repo) second call: %v", err)
	}
	if got := countDecodeFailureLogLines(logBuf.String()); got != 2 {
		t.Errorf("logDecodeFailureOnce lines after Invalidate + second call = %d, want 2 (fresh failure logs again): log=%q", got, logBuf.String())
	}
}

// countDecodeFailureLogLines counts logDecodeFailureOnce's own log lines
// within s, by exact line prefix rather than substring search: implDiag's
// own per-query "N candidate(s) skipped" summary line (see
// implementation_decodefailure_test.go) embeds the SAME wrapped decode
// error text mid-line via %v, so a plain substring count would double-count
// a single logDecodeFailureOnce call whenever a query's result is also
// empty enough to trigger that separate diagnostic.
func countDecodeFailureLogLines(s string) int {
	const prefix = "xref: typecheck: decode export data for"
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}
