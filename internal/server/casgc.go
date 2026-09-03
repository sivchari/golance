package server

import (
	"path/filepath"
	"time"

	"github.com/sivchari/golance/internal/store"
)

// otherDBOpenTimeout bounds how long RunCASGC waits to read another index
// database before giving up on including it in this round's mark set (see
// (*store.CAS).GC's safety doc). Short, since GC must never meaningfully
// delay whatever triggered it (server startup or a schema rebuild) and a
// database RunCASGC cannot open within this window is, by construction,
// exclusively locked by some other live process's writer handle (see
// store.DB's own doc on bbolt's whole-handle-lifetime lock) — waiting
// longer would not change the outcome for a genuinely busy writer, only
// delay GC for one that will still be busy on the next pass anyway.
const otherDBOpenTimeout = 50 * time.Millisecond

// indexDBGlobPattern matches every index database file — shared
// (indexDBFile) and session-private (privateIndexDBFile) alike — across
// every root golance has ever indexed on this machine, regardless of which
// CAS directory each one belongs to; RunCASGC filters by (*store.DB).CASDir
// after opening each candidate (see its own doc for why a filename alone
// cannot answer that).
const indexDBGlobPattern = "index-*.db"

// RunCASGC builds the mark set for casPath — the union of every
// UnitPointer.BlobKey recorded in ownDB (this call's own already-open
// database, read directly in process so it is never subject to
// otherDBOpenTimeout) plus every other index-*.db/*.private-*.db file in
// golance's cache directory whose own (*store.DB).CASDir matches casPath —
// then runs [store.CAS.GC] (force=true) or [store.CAS.MaybeGC]
// (force=false, throttled to once per [store.GCInterval]) against it,
// reporting the result via logf: one line, in the same key=value style as
// (*Server).instrument's slow-request line, only when a sweep actually ran
// (never for a MaybeGC call skipped by the interval throttle).
//
// ownPath is ownDB's own file path, skipped when enumerating other
// candidates: this process already holds it open (typically in write
// mode), so a second OpenReadOnly attempt against the very same file would
// simply time out for no benefit — ownDB's own contribution to the mark
// set is already collected directly.
//
// This never blocks a caller waiting on request handling or index
// building: every candidate database is opened with a short, bounded
// timeout (otherDBOpenTimeout), and the sweep itself never reads blob
// content (see (*store.CAS).GC's doc) — its cost is one directory walk of
// casPath plus, at most, len(other index databases) short bounded opens.
// logf is typically *log.Logger.Printf (a running server) or a thin
// fmt.Fprintf wrapper (the indexer subprocess — see cmd/golance's own
// caller); either way it is called at most once per invocation.
//
// stats/ran report exactly what the underlying GC/MaybeGC call reported
// (ran is always true when force is true), for a caller — or a test — that
// wants the outcome directly instead of parsing the log line.
func RunCASGC(logf func(format string, args ...any), casPath, ownPath string, ownDB *store.DB, force bool) (stats store.GCStats, ran bool) {
	if ownDB == nil {
		return store.GCStats{}, false
	}
	cas, err := store.OpenCAS(casPath)
	if err != nil {
		logf("golance: cas gc: open CAS %s: %v", casPath, err)
		return store.GCStats{}, false
	}

	marks := make(map[uint64]struct{})
	if err := ownDB.CollectBlobKeys(marks); err != nil {
		logf("golance: cas gc: collect own blob keys: %v", err)
		return store.GCStats{}, false
	}
	collectOtherCASMarks(casPath, ownPath, marks)

	now := time.Now()
	if force {
		stats, err = cas.GC(now, marks)
		ran = true
	} else {
		stats, ran, err = cas.MaybeGC(now, marks)
	}
	if err != nil {
		logf("golance: cas gc: %v", err)
		return store.GCStats{}, false
	}
	if !ran {
		return store.GCStats{}, false
	}
	logf("golance: cas gc: swept=%d swept_bytes=%d kept=%d kept_bytes=%d duration=%s",
		stats.SweptCount, stats.SweptBytes, stats.KeptCount, stats.KeptBytes, stats.Duration)
	return stats, true
}

// runStartupCASGC opportunistically sweeps root's CAS directory in the
// background: see RunCASGC's own doc for the full mark-and-sweep design.
// This is a no-op most of the time — store.GCInterval throttles the actual
// directory walk to once a day per CAS directory — and never blocks request
// handling or index building; loadWorkspaceAsync backgrounds this call via
// its own s.rpc.Go, separate from itself, specifically so it cannot delay
// anything else. A no-op if no index ended up open (tryWarmOpen/buildIndex
// both failed): there is nothing to build a mark set from.
func (s *Server) runStartupCASGC(root string) {
	idx := s.idx.Load()
	if idx == nil {
		return
	}
	RunCASGC(s.logger.Printf, casDir(root), s.dbPath(root), idx.db, false)
}

// collectOtherCASMarks adds every BlobKey recorded in every OTHER index
// database sharing casPath into marks, skipping ownPath (see RunCASGC's
// doc) and, silently, any candidate that cannot be opened read-only within
// otherDBOpenTimeout (a live writer) or that records no CASDir at all or a
// different one (a database predating this feature, or one for an
// unrelated repository) — see (*store.CAS).GC's safety doc for why an
// incomplete mark set here is a reclaim-speed tradeoff, not a correctness
// one.
func collectOtherCASMarks(casPath, ownPath string, marks map[uint64]struct{}) {
	matches, err := filepath.Glob(filepath.Join(cacheBaseDir(), "golance", indexDBGlobPattern))
	if err != nil {
		return
	}
	for _, path := range matches {
		if path == ownPath {
			continue
		}
		db, err := store.OpenReadOnlyTimeout(path, otherDBOpenTimeout)
		if err != nil {
			continue
		}
		if dir, err := db.CASDir(); err == nil && dir == casPath {
			_ = db.CollectBlobKeys(marks)
		}
		_ = db.Close()
	}
}
