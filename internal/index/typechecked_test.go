package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestBuild_RevertedContentIsCASHitNotTypeChecked verifies Stats.TypeChecked
// distinguishes an actual parse/type-check from a CAS hit: reverting a
// package's content to something the CAS has already seen (the in-process
// equivalent of `git checkout` back to a previously-built branch) must
// resolve without type-checking it again, even though the package's blob
// key no longer matches what db last recorded for it (so it is not a
// stat/content-hash skip either — see TestBuild_ContentChangeTriggersRebuild
// for that path).
func TestBuild_RevertedContentIsCASHitNotTypeChecked(t *testing.T) {
	dir := mutableTestModule(t)
	snap := loadSnapshot(t, dir)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("open root %s: %v", dir, err)
	}
	defer func() { _ = root.Close() }()

	leafRelPath := filepath.Join("leaf", "leaf.go")
	original, err := root.ReadFile(leafRelPath)
	if err != nil {
		t.Fatalf("read leaf.go: %v", err)
	}

	first, err := Build(ctx, snap, db, cas, Options{})
	if err != nil {
		t.Fatalf("first Build: %v", err)
	}
	if first.TypeChecked != 3 {
		t.Fatalf("first Build TypeChecked = %d, want 3 (empty CAS, everything is a fresh type-check)", first.TypeChecked)
	}

	edited := []byte(`// Package leaf has no workspace dependencies.
package leaf

// Greeting is a friendly greeting.
type Greeting struct {
	Message string
}

// String implements fmt.Stringer.
func (g Greeting) String() string {
	return g.Message
}

// Hello returns a Greeting for name, now with an exclamation mark.
func Hello(name string) Greeting {
	return Greeting{Message: "hello " + name + "!"}
}
`)
	if err := root.WriteFile(leafRelPath, edited, 0o600); err != nil {
		t.Fatalf("edit leaf.go: %v", err)
	}
	second, err := Build(ctx, snap, db, cas, Options{})
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if second.TypeChecked != 1 {
		t.Fatalf("second Build TypeChecked = %d, want 1 (leaf: new content the CAS has never seen)", second.TypeChecked)
	}

	if err := root.WriteFile(leafRelPath, original, 0o600); err != nil {
		t.Fatalf("revert leaf.go: %v", err)
	}
	third, err := Build(ctx, snap, db, cas, Options{})
	if err != nil {
		t.Fatalf("third Build: %v", err)
	}
	if third.TypeChecked != 0 {
		t.Errorf("third Build TypeChecked = %d, want 0 (leaf's original content is already in the CAS from the first Build)", third.TypeChecked)
	}
	if third.Processed != 1 {
		t.Errorf("third Build Processed = %d, want 1 (leaf: a CAS hit still counts as processed, just not type-checked)", third.Processed)
	}
}
