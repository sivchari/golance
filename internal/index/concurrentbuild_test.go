package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/graph"
)

// generateFanOutModule writes n root packages that each import every path
// in shared, so every worker in a parallel Build concurrently resolves and
// reads the SAME decoded dependency packages — the shape needed to exercise
// internal/index/facts.go's concurrent symbolID/objectpath extraction
// (addDefs/addRefs/registerMethodSet) against a shared *types.Package many
// goroutines touch at once.
func generateFanOutModule(t *testing.T, dir string, n int, shared []string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/fanout\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	for i := 0; i < n; i++ {
		pkgDir := filepath.Join(dir, fmt.Sprintf("pkg%d", i))
		if err := os.MkdirAll(pkgDir, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", pkgDir, err)
		}
		src := fmt.Sprintf("package pkg%d\n\nimport (\n", i)
		for _, s := range shared {
			src += fmt.Sprintf("\t%q\n", s)
		}
		src += ")\n\nfunc V() {\n"
		src += "\tvar d *json.Decoder\n\t_ = d\n"
		src += "\tvar h http.Handler\n\t_ = h\n"
		src += "\tvar l *slog.Logger\n\t_ = l\n"
		src += "\tvar tp *template.Template\n\t_ = tp\n"
		src += "\tvar pkg *types.Package\n\t_ = pkg\n"
		src += "\tvar decl *ast.GenDecl\n\t_ = decl\n"
		src += "\tvar db *sql.DB\n\t_ = db\n"
		src += "\tvar cfg *tls.Config\n\t_ = cfg\n"
		src += "}\n"
		if err := os.WriteFile(filepath.Join(pkgDir, "p.go"), []byte(src), 0o600); err != nil {
			t.Fatalf("write pkg%d/p.go: %v", i, err)
		}
	}
}

// TestBuild_ConcurrentWorkersSharingDecodedDepsIsRaceFree builds many root
// packages that all import the same handful of type-rich stdlib
// dependencies, at a parallelism high enough that many workers are
// resolving and extracting facts from those same SHARED decoded
// *types.Package values at once (see typecheck.Importer.decode and
// internal/index/facts.go's symbolID/objectpath.Encoder.For, both of which
// read a dependency's decoded types concurrently across workers by design —
// see Build's own doc on peak-memory-proportional-to-worker-count). Run
// under `go test -race`: a data race here would be a genuine defect in that
// sharing, not anything specific to depcheck's export-production path (see
// internal/depexport's own round-trip-decode invariant test for that
// side) — this test exists to confirm the concurrent-build architecture
// itself stays race-free against a realistic multi-package, multi-worker
// fan-in onto shared dependencies.
func TestBuild_ConcurrentWorkersSharingDecodedDepsIsRaceFree(t *testing.T) {
	const n = 80
	dir := t.TempDir()
	generateFanOutModule(t, dir, n, []string{
		"encoding/json", "net/http", "log/slog", "text/template",
		"go/types", "go/ast", "database/sql", "crypto/tls",
	})

	snap, err := graph.Load(graph.Options{Dir: dir}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	db := openTestDB(t)
	cas := openTestCAS(t)

	stats, err := Build(context.Background(), snap, db, cas, &Options{Parallelism: 16})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if stats.Errors != 0 {
		t.Errorf("stats.Errors = %d, want 0", stats.Errors)
	}
	if stats.Processed != n {
		t.Errorf("stats.Processed = %d, want %d", stats.Processed, n)
	}
}
