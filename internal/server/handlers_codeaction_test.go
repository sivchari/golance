package server

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAddImportFixes_ResolverErrorIsLogged pins the M9 fix: a genuine
// facts-index read error from importCandidates (here, the underlying db
// having gone away) used to be treated identically to "no import
// candidates exist for this name," leaving addImportFixes' "undefined: X"
// quickfix silently empty with no trace of what actually went wrong. It
// must now be logged, so the difference between "genuinely nothing to
// import" and "the index is broken" is not lost.
func TestAddImportFixes_ResolverErrorIsLogged(t *testing.T) {
	s, _, root := newTestServer(t)
	var logBuf bytes.Buffer
	s.logger = log.New(&logBuf, "", 0)

	idx := s.idx.Load()
	if idx == nil {
		t.Fatal("s.idx.Load() = nil, want a populated indexState")
	}
	if err := idx.db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	path := filepath.Join(root, "greet", "greet.go")
	text, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	actions := s.addImportFixes(path, text, 0, "SomeUndefinedName")
	if actions != nil {
		t.Errorf("addImportFixes = %+v, want nil (a closed index cannot produce real candidates)", actions)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "import candidates") {
		t.Errorf("log output = %q, want it to mention the import-candidates lookup failure", logged)
	}
	if !strings.Contains(logged, "SomeUndefinedName") {
		t.Errorf("log output = %q, want it to name the undefined identifier being resolved", logged)
	}
}
