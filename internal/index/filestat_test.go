package index

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sivchari/golance/internal/store"
)

func writeTempFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestStatFiles(t *testing.T) {
	dir := t.TempDir()
	a := writeTempFile(t, dir, "a.go", []byte("package a\n"))
	b := writeTempFile(t, dir, "b.go", []byte("package a\n\nvar X = 1\n"))

	got, err := statFiles([]string{a, b}, "", false)
	if err != nil {
		t.Fatalf("statFiles: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("statFiles returned %d entries, want 2", len(got))
	}
	for i, path := range []string{a, b} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got[i].Path != path || got[i].Size != fi.Size() || got[i].ModTimeNanos != fi.ModTime().UnixNano() {
			t.Errorf("statFiles()[%d] = %+v, want path=%s size=%d mtime=%d", i, got[i], path, fi.Size(), fi.ModTime().UnixNano())
		}
	}
}

func TestStatFiles_MissingFileErrors(t *testing.T) {
	if _, err := statFiles([]string{filepath.Join(t.TempDir(), "does-not-exist.go")}, "", false); err == nil {
		t.Error("statFiles() with a missing file = nil error, want an error")
	}
}

func TestFilesStatMatch(t *testing.T) {
	dir := t.TempDir()
	a := writeTempFile(t, dir, "a.go", []byte("package a\n"))
	stored, err := statFiles([]string{a}, "", false)
	if err != nil {
		t.Fatalf("statFiles: %v", err)
	}

	if !filesStatMatch([]string{a}, stored, "", false) {
		t.Error("filesStatMatch() = false for an untouched file, want true")
	}

	// Content changed, size differs (a stand-in for any real edit): mtime
	// also advances since the file is rewritten, but size alone already
	// disqualifies a match.
	if err := os.WriteFile(a, []byte("package a\n\nvar X = 1\n"), 0o600); err != nil {
		t.Fatalf("rewrite a.go: %v", err)
	}
	if filesStatMatch([]string{a}, stored, "", false) {
		t.Error("filesStatMatch() = true after the file's size changed, want false")
	}

	// Same size and content, but a different mtime (a touch/checkout).
	same := writeTempFile(t, dir, "same-size.go", []byte("package a\n"))
	storedSame, err := statFiles([]string{same}, "", false)
	if err != nil {
		t.Fatalf("statFiles: %v", err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(same, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if filesStatMatch([]string{same}, storedSame, "", false) {
		t.Error("filesStatMatch() = true after mtime changed, want false (mtime alone must not match)")
	}
}

func TestFilesStatMatch_FileCountMismatch(t *testing.T) {
	dir := t.TempDir()
	a := writeTempFile(t, dir, "a.go", []byte("package a\n"))
	b := writeTempFile(t, dir, "b.go", []byte("package a\n"))
	stored, err := statFiles([]string{a}, "", false)
	if err != nil {
		t.Fatalf("statFiles: %v", err)
	}

	if filesStatMatch([]string{a, b}, stored, "", false) {
		t.Error("filesStatMatch() = true after a file was added, want false (file count mismatch)")
	}
	if filesStatMatch([]string{}, stored, "", false) {
		t.Error("filesStatMatch() = true after every file was removed, want false")
	}
}

func TestFilesStatMatch_RenamedFileMismatch(t *testing.T) {
	// Same file count, same total content, but a different path per file:
	// a rename must not be mistaken for "unchanged".
	dir := t.TempDir()
	a := writeTempFile(t, dir, "a.go", []byte("package a\n"))
	stored, err := statFiles([]string{a}, "", false)
	if err != nil {
		t.Fatalf("statFiles: %v", err)
	}
	renamed := filepath.Join(dir, "renamed.go")
	if err := os.Rename(a, renamed); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if filesStatMatch([]string{renamed}, stored, "", false) {
		t.Error("filesStatMatch() = true after a rename, want false")
	}
}

func TestFilesStatMatch_MissingFile(t *testing.T) {
	stored := []store.FileStat{{Path: filepath.Join(t.TempDir(), "gone.go"), Size: 1, ModTimeNanos: 1}}
	if filesStatMatch([]string{stored[0].Path}, stored, "", false) {
		t.Error("filesStatMatch() = true for a file that no longer exists, want false")
	}
}
