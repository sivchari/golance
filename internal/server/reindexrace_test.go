package server

import (
	"bytes"
	"context"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/xref"
)

// newReindexRaceServer builds a Server wired over testdata/module exactly
// like newTestServer, except its logger is backed by buf instead of
// t.Logf — so a test that deliberately races a detached reindex against a
// concurrent index close can assert directly on what (if anything) got
// logged, rather than depending on a background goroutine finishing before
// the test function itself returns (see server_test.go's testLogWriter,
// which panics if Write is called afterward — exactly the failure mode
// this whole file exists to pin down without ever risking it).
func newReindexRaceServer(t *testing.T) (s *Server, ws *workspace, idx *indexState, buf *bytes.Buffer) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cas, err := store.OpenCAS(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	if _, err := index.Build(context.Background(), snap, db, cas, &index.Options{}); err != nil {
		t.Fatalf("index.Build: %v", err)
	}

	buf = &bytes.Buffer{}
	logger := log.New(buf, "", 0)
	rpcServer := rpc.NewServer(rpc.WithLogger(logger))
	s = New(rpcServer, Options{Logger: logger})
	s.setWorkspace(root, snap)
	idx = &indexState{db: db, cas: cas, resolver: xref.New(db, cas, snap, false)}
	s.idx.Store(idx)

	ws = s.workspace()
	return s, ws, idx, buf
}

const reindexRaceTestPkg = "example.com/servermod/greet"

// TestBeginReindex_RejectsOnceIndexSuperseded verifies beginReindex's own
// freshness check: it registers (ok=true) against the server's actual
// current index, and refuses (ok=false, nothing registered) once s.idx no
// longer points at that same database — the exact condition
// revalidateIndex's rebuild branch creates via Store(nil) before it ever
// reaches Close (see indexer.go).
func TestBeginReindex_RejectsOnceIndexSuperseded(t *testing.T) {
	s, _, idx, _ := newReindexRaceServer(t)

	if !s.beginReindex(idx) {
		t.Fatal("beginReindex() against the server's own current index = false, want true")
	}
	s.reindexWG.Done()

	s.idx.Store(nil)
	if s.beginReindex(idx) {
		t.Error("beginReindex() after s.idx.Store(nil) = true, want false (must not register against a superseded index)")
	}
}

// TestReindexWG_RebuildCloseWaitsForRegisteredReindex is the core
// regression test for the mechanism fixed in documentsync.go/indexer.go: a
// reindex already registered via beginReindex must complete — and only
// then may revalidateIndex's rebuild branch proceed past its own
// s.reindexWG.Wait() to Store(nil)-and-Close the database that write is
// using. Before reindexWG existed, nothing prevented that Close from
// running concurrently with (or strictly before) a detached reindex's own
// persist call, producing the "database not open" failure
// TestDidOpenDidChangeReflectedInHover could intermittently panic on (a
// background reindex logging via t.Logf after the test itself had already
// returned) — reproducible before this fix via
// `go test -race -run TestDidOpenDidChangeReflectedInHover -count=50`.
//
// This test drives the exact same sequence deterministically instead of
// relying on goroutine-scheduling luck: register, confirm the simulated
// rebuild's Wait() blocks, run the real write while it is still blocked,
// signal completion, and confirm Wait() only then unblocks — with the
// close that follows succeeding on a database no in-flight write could
// still be touching.
func TestReindexWG_RebuildCloseWaitsForRegisteredReindex(t *testing.T) {
	s, ws, idx, buf := newReindexRaceServer(t)

	if !s.beginReindex(idx) {
		t.Fatal("beginReindex() = false, want true")
	}

	waitDone := make(chan struct{})
	go func() {
		s.reindexWG.Wait()
		close(waitDone)
	}()

	select {
	case <-waitDone:
		t.Fatal("reindexWG.Wait() returned before the registered reindex called Done — a concurrent rebuild could have closed idx.db while the write below was still in flight")
	case <-time.After(50 * time.Millisecond):
		// Still correctly blocked: safe to perform the registered write now.
	}

	s.reindex(context.Background(), ws, idx, reindexRaceTestPkg)
	s.reindexWG.Done()

	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("reindexWG.Wait() did not return after the registered reindex's own Done()")
	}

	if err := idx.db.Close(); err != nil {
		t.Errorf("close after the registered write completed: %v", err)
	}
	if buf.String() != "" {
		t.Errorf("unexpected log output during a clean registered write: %q", buf.String())
	}
}

// TestReindex_DatabaseClosedExternally_SkipsSilently pins
// reindexDBClosedUnderfoot's own reason to exist: idx.db can be closed by
// code entirely outside this package's control — concretely, a test
// harness that owns the same *store.DB handle newTestServer wires into
// s.idx and closes it via t.Cleanup, exactly as
// TestDidOpenDidChangeReflectedInHover's own fixture does — without ever
// going through revalidateIndex's Store(nil)/reindexWG.Wait()/Close
// sequence beginReindex otherwise serializes against. reindex must still
// treat the resulting persist failure as the same benign, self-healing
// outcome reindexIfStillCurrent's own bail-out is (see its doc): no log
// line (the one difference from an ordinary reindex failure — see
// reindex's own error branch), since a background goroutine logging after
// its owning test has already returned is exactly what panics
// testLogWriter.Write in server_test.go.
func TestReindex_DatabaseClosedExternally_SkipsSilently(t *testing.T) {
	s, ws, idx, buf := newReindexRaceServer(t)

	if err := idx.db.Close(); err != nil {
		t.Fatalf("close idx.db: %v", err)
	}

	// s.idx still points at idx — nothing told the server this specific
	// database handle was closed, exactly like the race this test pins.
	s.reindex(context.Background(), ws, idx, reindexRaceTestPkg)

	if buf.String() != "" {
		t.Errorf("reindex against an externally-closed database logged %q, want silence (self-heals on the next trigger — see reindexDBClosedUnderfoot's doc)", buf.String())
	}
}
