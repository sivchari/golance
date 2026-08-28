package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sivchari/golance/internal/graph"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-version"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(-version) code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "golance") {
		t.Fatalf("run(-version) stdout = %q, want it to mention golance", stdout.String())
	}
}

func TestRunUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-does-not-exist"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("run(-does-not-exist) code = %d, want 2", code)
	}
}

func TestRunUnexpectedArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"bogus-positional-arg"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("run(bogus-positional-arg) code = %d, want 2", code)
	}
}

func TestRunServesUntilEOF(t *testing.T) {
	// No "initialize" sent: an empty client stream should make Serve
	// return cleanly (EOF), and run should report exit code 0.
	var stdout, stderr bytes.Buffer
	code := run(nil, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
}

func TestRunIndexerRequiresEnv(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runIndexer(&stdout, &stderr)
	if code != 1 {
		t.Fatalf("runIndexer() with no env set: code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "GOLANCE_ROOT") {
		t.Fatalf("runIndexer() stderr = %q, want it to mention GOLANCE_ROOT", stderr.String())
	}
}

// writeTinyModule writes a minimal single-package module to a fresh temp
// directory and returns its path.
func writeTinyModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/tiny\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tiny.go"), []byte("package tiny\n\n// V returns 1.\nfunc V() int { return 1 }\n"), 0o600); err != nil {
		t.Fatalf("write tiny.go: %v", err)
	}
	return dir
}

// TestLoadGraph_ReusesCacheWhenNotStale verifies that a second loadGraph
// call for the same root reuses the on-disk graph cache (graph.LoadCache)
// instead of re-running `go list`, and that touching go.mod (making the
// cache stale per graph.Stale) forces a fresh load again.
func TestLoadGraph_ReusesCacheWhenNotStale(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := writeTinyModule(t)
	opts := graph.Options{Dir: dir}
	patterns := []string{"./..."}
	var stderr bytes.Buffer

	snap1, fromCache1, err := loadGraph(opts, patterns, &stderr)
	if err != nil {
		t.Fatalf("loadGraph (first): %v", err)
	}
	if fromCache1 {
		t.Error("loadGraph (first) fromCache = true, want false (nothing cached yet)")
	}
	if len(snap1.Packages) == 0 {
		t.Fatal("loadGraph (first) returned an empty snapshot")
	}

	_, fromCache2, err := loadGraph(opts, patterns, &stderr)
	if err != nil {
		t.Fatalf("loadGraph (second): %v", err)
	}
	if !fromCache2 {
		t.Error("loadGraph (second) fromCache = false, want true (cache should be reused)")
	}

	// Touching go.mod must invalidate the cache (graph.Stale compares
	// mtimes, so the new mtime must be strictly later).
	goMod := filepath.Join(dir, "go.mod")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(goMod, future, future); err != nil {
		t.Fatalf("Chtimes(go.mod): %v", err)
	}

	_, fromCache3, err := loadGraph(opts, patterns, &stderr)
	if err != nil {
		t.Fatalf("loadGraph (after touching go.mod): %v", err)
	}
	if fromCache3 {
		t.Error("loadGraph (after touching go.mod) fromCache = true, want false (cache must be stale)")
	}
}
