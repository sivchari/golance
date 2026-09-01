package server

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

// TestBuildPkgNameIndex checks that every package testdata/module's graph
// loads — workspace packages and their standard-library dependencies alike
// — is indexed by its declared name, the candidate source for both
// unimported-completion shapes.
func TestBuildPkgNameIndex(t *testing.T) {
	_, snap, _ := newTestServer(t)
	idx := buildPkgNameIndex(snap)

	want := map[string]string{
		"greet":      "example.com/servermod/greet",
		"unimported": "example.com/servermod/unimported",
		"depuse":     "example.com/servermod/depuse",
		"fmt":        "fmt",
		"strings":    "strings",
	}
	for name, path := range want {
		paths, ok := idx[name]
		if !ok {
			t.Errorf("pkgNameIndex missing name %q", name)
			continue
		}
		if !slices.Contains(paths, path) {
			t.Errorf("pkgNameIndex[%q] = %v, want to contain %q", name, paths, path)
		}
	}
}

// TestUnimportedPackageCandidates_Cap checks that unimportedPackageCandidates
// never returns more than maxUnimportedPackages candidates, in
// deterministic (name, then import path) order — computed against a
// synthetic workspace so the assertion does not depend on how many
// name-matching packages testdata/module happens to have.
func TestUnimportedPackageCandidates_Cap(t *testing.T) {
	s, _, root := newTestServer(t)
	cp, err := s.workspace().engine.Get(context.Background(), filepath.Join(root, "unimported", "unimported.go"))
	if err != nil {
		t.Fatalf("engine.Get: %v", err)
	}

	synth := &workspace{pkgNameIndex: map[string][]string{
		"za": {"example.com/synth/za"},
		"zb": {"example.com/synth/zb"},
		"zc": {"example.com/synth/zc"},
		"zd": {"example.com/synth/zd"},
		"ze": {"example.com/synth/ze"},
		"zf": {"example.com/synth/zf"},
	}}

	got := unimportedPackageCandidates(synth, cp, "z")
	if len(got) != maxUnimportedPackages {
		t.Fatalf("len(candidates) = %d, want %d (cap)", len(got), maxUnimportedPackages)
	}
	want := []string{"za", "zb", "zc", "zd", "ze"}
	for i, c := range got {
		if c.Name != want[i] {
			t.Errorf("candidates[%d].Name = %q, want %q (deterministic, alphabetical order)", i, c.Name, want[i])
		}
	}
}

// TestUnimportedPackageCandidates_ExcludesAlreadyImported checks that a
// package cp's file already imports is never offered again as an
// "unimported" candidate — see importedPackagePaths.
func TestUnimportedPackageCandidates_ExcludesAlreadyImported(t *testing.T) {
	s, _, root := newTestServer(t)
	ws := s.workspace()
	cp, err := ws.engine.Get(context.Background(), filepath.Join(root, "depuse", "depuse.go"))
	if err != nil {
		t.Fatalf("engine.Get: %v", err)
	}

	got := unimportedPackageCandidates(ws, cp, "s")
	for _, c := range got {
		if c.Name == "strings" {
			t.Errorf("candidates = %+v, want \"strings\" excluded: depuse.go already imports it", got)
		}
	}
}

// TestUnimportedPackageCandidates_ExcludesOwnPackage checks that
// unimportedPackageCandidates never suggests importing the package cp
// itself belongs to, even if its name happens to match prefix.
func TestUnimportedPackageCandidates_ExcludesOwnPackage(t *testing.T) {
	s, _, root := newTestServer(t)
	ws := s.workspace()
	cp, err := ws.engine.Get(context.Background(), filepath.Join(root, "depuse", "depuse.go"))
	if err != nil {
		t.Fatalf("engine.Get: %v", err)
	}

	got := unimportedPackageCandidates(ws, cp, "depuse")
	for _, c := range got {
		if c.ImportPath == cp.PkgPath() {
			t.Errorf("candidates = %+v, want cp's own package (%s) excluded", got, cp.PkgPath())
		}
	}
}
