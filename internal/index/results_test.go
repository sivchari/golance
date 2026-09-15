package index

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/store"
)

// TestBuildResults_BatchCommitFailureIsNotFatal is the regression test for
// the cold-start double-build bug traced to flushPendingLocked: a
// PutUnitsBatch commit failure used to become Build's own fatal error (via
// addErrLocked), which withheld [store.DB.PutBuildFingerprint] entirely —
// so a database that finished processing every package (progress reached
// 100%) but lost its very last batch to a transient write failure (disk
// pressure, memory pressure — anything, not just the literal "no space left
// on device" this was reproduced with against a real indexer subprocess on
// a mid-size corpus) came out of Build with NO recorded build fingerprint.
// internal/server's revalidateIndex/index.RevalidateStale then judged the
// whole database wholeDBStale on the very next call (immediately after
// buildIndex, see loadWorkspaceAsync) and forced a full close-and-rebuild —
// which itself races the still-resident memory from the first build,
// exactly the OOM-prone double build a real cold start against a 2,573-
// package monorepo was observed doing. Closing db before flush forces a
// real PutUnitsBatch failure without needing a real disk-full or
// out-of-memory condition to reproduce it.
func TestBuildResults_BatchCommitFailureIsNotFatal(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	r := newBuildResults(db, 50)
	r.pending = []store.UnitEntry{{PkgHash: store.Hash("example.com/broken/pkg")}}

	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	r.flush()

	stats, resultErr := r.result()
	if resultErr != nil {
		t.Errorf("result() err = %v, want nil (a batch commit failure must not become Build's own fatal error, or PutBuildFingerprint would never run)", resultErr)
	}
	if stats.Errors != 1 {
		t.Errorf("stats.Errors = %d, want 1", stats.Errors)
	}
	if got := buf.String(); !strings.Contains(got, "failed to commit a batch") {
		t.Errorf("log output = %q, want it to report the batch commit failure", got)
	}
}

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
