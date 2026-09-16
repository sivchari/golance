package depcheck

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestProvider_Package_SkipsCgoFiles pins the cgo handling check's parse
// loop applies: a file importing the pseudo-package "C" cannot be
// declaration-checked from raw source (observed on GOOS=linux, where
// net/cgo_linux.go marked net — and transitively net/http and crypto/tls —
// Incomplete), so it is skipped, losing only its own declarations while the
// rest of the package still checks cleanly. Hermetic: the fixture carries
// its own cgo file, so the assertion holds on every GOOS regardless of the
// host toolchain's build-tag selection.
func TestProvider_Package_SkipsCgoFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}
	plain := write("plain.go", "package cgomix\n\n// Plain is declared in a normal file.\nfunc Plain() int { return 1 }\n")
	cgo := write("cgo.go", "package cgomix\n\n/*\n#include <stdlib.h>\n*/\nimport \"C\"\n\n// FromCgo leans on cgo preprocessing this checker never runs.\nfunc FromCgo() { C.free(nil) }\n")

	meta := cgoStubMetadata{"example.com/cgomix": {dir: dir, goFiles: []string{plain, cgo}}}
	p := NewProvider(meta, Options{})

	cp, err := p.Package(context.Background(), "example.com/cgomix")
	if err != nil {
		t.Fatalf("Package(cgomix): %v", err)
	}
	if cp.Incomplete() {
		t.Fatalf("Package(cgomix).Incomplete() = true, want false (cgo file must be skipped, not fail the check): FirstError=%q", cp.FirstError())
	}
	if cp.Types().Scope().Lookup("Plain") == nil {
		t.Error(`checked package lacks "Plain" from the non-cgo file`)
	}
	if cp.Types().Scope().Lookup("FromCgo") != nil {
		t.Error(`checked package unexpectedly contains "FromCgo" from the skipped cgo file`)
	}
}

// TestProvider_Package_OnlyCgoFilesFailsCleanly pins the all-cgo edge: with
// every file skipped there is nothing to check, and the caller gets a clear
// error naming the package instead of a zero-file panic or a silently empty
// result.
func TestProvider_Package_OnlyCgoFilesFailsCleanly(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "only.go")
	if err := os.WriteFile(p, []byte("package onlycgo\n\nimport \"C\"\n\nfunc F() { C.free(nil) }\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	meta := cgoStubMetadata{"example.com/onlycgo": {dir: dir, goFiles: []string{p}}}

	_, err := NewProvider(meta, Options{}).Package(context.Background(), "example.com/onlycgo")
	if err == nil {
		t.Fatal("Package(onlycgo) error = nil, want an error for a package with only cgo files")
	}
}

// cgoStubMetadata is a minimal MetadataSource over literal file lists, so
// the cgo fixtures need no go.mod/graph.Load.
type cgoStubMetadata map[string]cgoStubPkg

type cgoStubPkg struct {
	dir     string
	goFiles []string
}

func (m cgoStubMetadata) Package(pkgPath string) (string, []string, []string, bool) {
	p, ok := m[pkgPath]
	if !ok {
		return "", nil, nil, false
	}
	return p.dir, p.goFiles, nil, true
}
