package xref

import (
	"context"
	"strings"
	"testing"
)

// TestResolveAt_DependencyFileDegradesWithoutHardError pins the second
// fix's own core change (see New's doc): a position inside a dependency
// (module cache or GOROOT) file must resolve the same low-noise way as any
// other file outside the facts index's coverage ("not part of any known
// package"), rather than being found by pkgPathForFile's directory
// fallback — snap.Packages carries the whole transitive closure, so
// without this exclusion a dependency file's real pkgPath resolves fine —
// only to send resolveAt straight into a GetUnit call that can never
// succeed, since the facts index only ever covers root (workspace)
// packages (internal/index/scheduler.go's doc). A real monorepo report
// traced exactly that dead-end error ("xref: read facts for gorm.io/gorm:
// store: not found") through server logging; internal/server.
// definitionFallback's ad-hoc CheckedPackage/SamePackageDefinition/
// DependencyDefinition chain already answers a dependency-file query
// correctly regardless (verified separately at the server layer), so the
// only thing worth pinning here is that resolveAt itself no longer reports
// what looks like a genuine index failure for an entirely expected case.
func TestResolveAt_DependencyFileDegradesWithoutHardError(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/depfile\n\ngo 1.23\n")
	writeTestFile(t, dir, "app/app.go", `package app

import "fmt"

func Describe(n int) string {
	return fmt.Sprintf("%d", n)
}
`)

	r, snap := newResolverForDir(t, dir)
	fmtPkg, ok := snap.Package("fmt")
	if !ok || len(fmtPkg.GoFiles) == 0 {
		t.Fatal("fmt package not found in loaded snapshot (stdlib should always resolve)")
	}
	if fmtPkg.Root {
		t.Fatal("fmt reported as a root (workspace) package — test assumption broken")
	}
	fmtFile := fmtPkg.GoFiles[0]

	_, err := r.resolveAt(context.Background(), fmtFile, 1, 1)
	if err == nil {
		t.Fatal("resolveAt succeeded for a position in a dependency file, want an error (never indexed)")
	}
	msg := err.Error()
	if !strings.Contains(msg, "is not part of any known package") {
		t.Errorf("resolveAt error = %q, want it to read like an ordinary unindexed-file miss (\"is not part of any known package\"), not a facts-read failure", msg)
	}
	if strings.Contains(msg, "store:") || strings.Contains(msg, "read facts for") {
		t.Errorf("resolveAt error = %q, want no facts-index-lookup wording at all: dependency files should never reach that code path", msg)
	}
}

// TestDefinition_DependencyFileReturnsCleanMiss is
// TestResolveAt_DependencyFileDegradesWithoutHardError's Definition-level
// counterpart: the public entry point internal/server.handleDefinition
// actually calls must behave the same way, since its own fallback decision
// (definitionFallback) hinges on Definition returning any error at all, not
// on its specific wording.
func TestDefinition_DependencyFileReturnsCleanMiss(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/depfiledef\n\ngo 1.23\n")
	writeTestFile(t, dir, "app/app.go", `package app

import "fmt"

func Describe(n int) string {
	return fmt.Sprintf("%d", n)
}
`)

	r, snap := newResolverForDir(t, dir)
	fmtPkg, ok := snap.Package("fmt")
	if !ok || len(fmtPkg.GoFiles) == 0 {
		t.Fatal("fmt package not found in loaded snapshot")
	}

	if _, err := r.Definition(context.Background(), fmtPkg.GoFiles[0], 1, 1); err == nil {
		t.Fatal("Definition succeeded for a position in a dependency file, want an error (caller falls back to definitionFallback on any error)")
	}
}
