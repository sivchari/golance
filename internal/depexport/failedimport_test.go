package depexport

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/depcheck"
)

// sharedMissingMetadataSource reports "top", importing "q1" and "q2", which
// both import the same path this source does not know at all — the exact
// shape of the production failure this file regresses (two members of one
// dependency closure importing the same unresolvable path).
type sharedMissingMetadataSource struct {
	dir string
}

func (s sharedMissingMetadataSource) Package(pkgPath string) (dir string, goFiles, imports []string, ok bool) {
	switch pkgPath {
	case "top":
		return s.dir, []string{filepath.Join(s.dir, "top.go")}, []string{"q1", "q2"}, true
	case "q1":
		return s.dir, []string{filepath.Join(s.dir, "q1.go")}, []string{"missing"}, true
	case "q2":
		return s.dir, []string{filepath.Join(s.dir, "q2.go")}, []string{"missing"}, true
	default:
		return "", nil, nil, false
	}
}

func writeSharedMissingFixture(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"top.go": "package top\n\nimport (\n\t_ \"q1\"\n\t_ \"q2\"\n)\n",
		"q1.go":  "package q1\n\nimport \"missing\"\n\n// V references the unresolvable import.\nvar V = missing.V\n",
		"q2.go":  "package q2\n\nimport \"missing\"\n\n// V references the unresolvable import.\nvar V = missing.V\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// TestCache_ExportData_SharedUnresolvableImportIsNotDuplicate is the
// regression test for the field failure observed against
// github.com/snowflakedb/gosnowflake/v2: two packages in one closure
// importing the same unresolvable path used to each embed their own
// go/types-fabricated fake package for it, so ExportData failed hard with
// "would reference two non-identical packages both named ..." instead of
// returning the same degraded-but-usable (complete=false, never persisted)
// result any other transiently-broken closure gets.
func TestCache_ExportData_SharedUnresolvableImportIsNotDuplicate(t *testing.T) {
	dir := t.TempDir()
	writeSharedMissingFixture(t, dir)
	meta := sharedMissingMetadataSource{dir: dir}
	provider := depcheck.NewProvider(meta, depcheck.Options{})
	cas := newTestCAS(t)
	cache := NewCache(cas, meta, provider, Options{GOROOT: dir})

	data, complete, ok, err := cache.ExportDataComplete("top")
	if err != nil {
		t.Fatalf("ExportDataComplete(top): %v", err)
	}
	if !ok {
		t.Fatal("ExportDataComplete(top): ok = false, want true")
	}
	if complete {
		t.Error("ExportDataComplete(top): complete = true for a closure with an unresolvable import, want false")
	}
	if len(data) == 0 {
		t.Error("ExportDataComplete(top): empty data")
	}

	key := cache.digest("top", dir)
	if cas.Has(key) {
		t.Error("CAS holds a blob for an incomplete closure; it must never be persisted")
	}
}
