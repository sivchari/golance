package index

import (
	"bytes"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
)

// TestBuildResults_RecordLogsPerPackageError verifies that a per-package
// processUnit failure is logged naming the failing package and the error,
// via the standard logger — the same mechanism processUnitRecovered's
// panic recovery already uses (see its doc). Before this, record only
// counted the failure into Stats.Errors and otherwise discarded err
// entirely: nothing anywhere named which package failed or why, and
// cmd/golance's indexer subprocess exits 0 for this case (Build's own
// contract keeps a per-package error out of its returned error — see
// record's doc), so the server discarding that subprocess's stderr left no
// trace of the failure at all.
func TestBuildResults_RecordLogsPerPackageError(t *testing.T) {
	db := openTestDB(t)
	r := newBuildResults(db, 50)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	wantErr := errors.New("boom: type-check failed")
	const wantPkg = "example.com/broken/pkg"
	r.record(wantPkg, nil, false, false, wantErr)

	got := buf.String()
	if !strings.Contains(got, wantPkg) {
		t.Errorf("log output = %q, want it to name the failing package %q", got, wantPkg)
	}
	if !strings.Contains(got, wantErr.Error()) {
		t.Errorf("log output = %q, want it to include the error %q", got, wantErr.Error())
	}

	stats, err := r.result()
	if err != nil {
		t.Errorf("result() err = %v, want nil (a per-package error must not become Build's own fatal error)", err)
	}
	if stats.Errors != 1 {
		t.Errorf("stats.Errors = %d, want 1", stats.Errors)
	}
}
