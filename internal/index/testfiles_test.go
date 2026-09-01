package index

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sivchari/golance/internal/graph"
)

func TestTestFilesInPackage_FindsInPackageTestFile(t *testing.T) {
	dir := t.TempDir()
	nonTest := writeTempFile(t, dir, "a.go", []byte("package a\n"))
	testFile := writeTempFile(t, dir, "a_test.go", []byte("package a\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n"))

	pkg := &graph.Package{Dir: dir, GoFiles: []string{nonTest}}
	got := testFilesInPackage(pkg, readFileDisk)
	if !reflect.DeepEqual(got, []string{testFile}) {
		t.Errorf("testFilesInPackage() = %v, want [%s]", got, testFile)
	}
}

func TestTestFilesInPackage_ExcludesExternalTestPackage(t *testing.T) {
	dir := t.TempDir()
	nonTest := writeTempFile(t, dir, "a.go", []byte("package a\n"))
	writeTempFile(t, dir, "a_test.go", []byte("package a_test\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n"))

	pkg := &graph.Package{Dir: dir, GoFiles: []string{nonTest}}
	got := testFilesInPackage(pkg, readFileDisk)
	if len(got) != 0 {
		t.Errorf("testFilesInPackage() = %v, want none (an external \"_test\" package must contribute nothing)", got)
	}
}

func TestTestFilesInPackage_MultipleTestFilesSorted(t *testing.T) {
	dir := t.TempDir()
	nonTest := writeTempFile(t, dir, "a.go", []byte("package a\n"))
	z := writeTempFile(t, dir, "z_test.go", []byte("package a\n"))
	b := writeTempFile(t, dir, "b_test.go", []byte("package a\n"))

	pkg := &graph.Package{Dir: dir, GoFiles: []string{nonTest}}
	got := testFilesInPackage(pkg, readFileDisk)
	want := []string{b, z}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("testFilesInPackage() = %v, want %v (sorted)", got, want)
	}
}

// TestTestFilesInPackage_UnresolvableCanonicalNameDegradesToNil verifies
// that testFilesInPackage degrades to no test files (rather than an error)
// when it cannot even determine the package's own canonical name — here
// because pkg.GoFiles names a file that does not exist.
func TestTestFilesInPackage_UnresolvableCanonicalNameDegradesToNil(t *testing.T) {
	pkg := &graph.Package{Dir: t.TempDir(), GoFiles: []string{filepath.Join(t.TempDir(), "does-not-exist.go")}}
	got := testFilesInPackage(pkg, readFileDisk)
	if got != nil {
		t.Errorf("testFilesInPackage() = %v, want nil when the canonical package name cannot be resolved", got)
	}
}

func TestIsTestGoFile(t *testing.T) {
	cases := map[string]bool{
		"a_test.go":  true,
		"_a_test.go": false,
		".a_test.go": false,
		"a.go":       false,
		"a_test.txt": false,
	}
	for name, want := range cases {
		if got := isTestGoFile(name); got != want {
			t.Errorf("isTestGoFile(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestEffectiveGoFiles(t *testing.T) {
	goFiles := []string{"a.go", "b.go"}
	if got := effectiveGoFiles(goFiles, nil); !reflect.DeepEqual(got, goFiles) {
		t.Errorf("effectiveGoFiles(goFiles, nil) = %v, want %v unchanged", got, goFiles)
	}
	got := effectiveGoFiles(goFiles, []string{"a_test.go"})
	want := []string{"a.go", "b.go", "a_test.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("effectiveGoFiles() = %v, want %v", got, want)
	}
}
