package graph

import (
	"path/filepath"
	"testing"
)

// TestBuildFlagsFingerprint_Deterministic verifies that hashing the same
// buildFlags and env twice produces the same fingerprint, and that a
// different buildFlags value changes it — the property
// internal/index.Options.BuildFlagsFingerprint (folded into each package's
// content hash) needs to actually invalidate the index on a real build-flags
// change (see H3).
func TestBuildFlagsFingerprint_Deterministic(t *testing.T) {
	env := []string{"GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=1"}

	a := buildFlagsFingerprint([]string{"-tags=foo"}, env)
	b := buildFlagsFingerprint([]string{"-tags=foo"}, env)
	if a != b {
		t.Errorf("buildFlagsFingerprint is not deterministic: %q != %q for identical inputs", a, b)
	}

	c := buildFlagsFingerprint([]string{"-tags=bar"}, env)
	if a == c {
		t.Errorf("buildFlagsFingerprint(%q) == buildFlagsFingerprint(%q) = %q, want distinct fingerprints for distinct build flags", "-tags=foo", "-tags=bar", a)
	}
}

// TestBuildFlagsFingerprint_ChangesWithTrackedEnvVars verifies each of
// GOFLAGS/GOOS/GOARCH/CGO_ENABLED independently changes the fingerprint,
// since any of them can change how the same file set type-checks (see H3's
// CGO_ENABLED/GOOS/GOARCH example).
func TestBuildFlagsFingerprint_ChangesWithTrackedEnvVars(t *testing.T) {
	base := buildFlagsFingerprint(nil, []string{"GOFLAGS=", "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0"})

	tests := []struct {
		name string
		env  []string
	}{
		{"GOFLAGS changes", []string{"GOFLAGS=-tags=x", "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0"}},
		{"GOOS changes", []string{"GOFLAGS=", "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0"}},
		{"GOARCH changes", []string{"GOFLAGS=", "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0"}},
		{"CGO_ENABLED changes", []string{"GOFLAGS=", "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildFlagsFingerprint(nil, tt.env)
			if got == base {
				t.Errorf("buildFlagsFingerprint unchanged after %s: %q", tt.name, got)
			}
		})
	}
}

// TestLoad_BuildFlagsFingerprint_DiffersByBuildFlags verifies the
// production integration, not just the hashing primitive: two Load calls
// against the same source with different Options.BuildFlags must produce
// snapshots whose BuildFlagsFingerprint differs, exactly what
// internal/index.Options.BuildFlagsFingerprint's default (see
// resolveBuildFlagsFingerprint in that package) needs to actually catch a
// GOFLAGS/-tags change.
func TestLoad_BuildFlagsFingerprint_DiffersByBuildFlags(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "simple"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}

	plain, err := Load(Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("Load(plain): %v", err)
	}
	tagged, err := Load(Options{Dir: root, BuildFlags: []string{"-tags=regressionh3"}}, "./...")
	if err != nil {
		t.Fatalf("Load(tagged): %v", err)
	}

	if plain.BuildFlagsFingerprint() == "" {
		t.Error("BuildFlagsFingerprint() = \"\", want a non-empty fingerprint")
	}
	if plain.BuildFlagsFingerprint() == tagged.BuildFlagsFingerprint() {
		t.Error("BuildFlagsFingerprint unchanged across a BuildFlags difference, want it to differ")
	}
}

// TestCache_RoundTrip_PreservesBuildFlagsFingerprint verifies SaveCache/
// LoadCache round-trip snap.BuildFlagsFingerprint(): without this, a
// snapshot restored from the on-disk graph cache (the normal cold-start
// path) would always report an empty fingerprint regardless of what it was
// when saved, silently reopening the H3 gap for every cache-hit reload.
func TestCache_RoundTrip_PreservesBuildFlagsFingerprint(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "simple"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	withTestCacheDir(t)

	snap := loadTestdata(t)
	if snap.BuildFlagsFingerprint() == "" {
		t.Fatal("loadTestdata's snapshot has an empty BuildFlagsFingerprint; test fixture is not exercising the property under test")
	}

	patterns := []string{"./..."}
	if err := SaveCache(root, patterns, nil, snap); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	loaded, ok := LoadCache(root, patterns, nil)
	if !ok {
		t.Fatal("LoadCache: ok=false after SaveCache")
	}
	if loaded.BuildFlagsFingerprint() != snap.BuildFlagsFingerprint() {
		t.Errorf("LoadCache BuildFlagsFingerprint() = %q, want %q (the value SaveCache was given)", loaded.BuildFlagsFingerprint(), snap.BuildFlagsFingerprint())
	}
}
