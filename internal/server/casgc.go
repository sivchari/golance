package server

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/sivchari/golance/internal/store"
)

// otherDBOpenTimeout bounds how long RunCASGC waits to read another index
// database before giving up on including it in this round's mark set (see
// collectOtherCASMarks's doc for what happens to the mark set when it does).
// Short, since GC must never meaningfully delay whatever triggered it
// (server startup or a schema rebuild) and a database RunCASGC cannot open
// within this window is, by construction, exclusively locked by some other
// live process's writer handle (see store.DB's own doc on bbolt's
// whole-handle-lifetime lock) — waiting longer would not change the outcome
// for a genuinely busy writer, only delay GC for one that will still be busy
// on the next pass anyway.
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
// The invariant this whole call must uphold: a blob may only be swept when
// every index database sharing casPath was actually read this round. A
// database this call could not open (otherDBOpenTimeout) or whose CASDir
// could not be read makes the mark set an under-approximation of what is
// genuinely still referenced — sweeping against it risks deleting a blob
// some other live session's UnitPointer still names, which nothing ever
// repairs (see collectOtherCASMarks's doc), unlike an ordinary CAS miss.
// RunCASGC therefore skips the sweep entirely — logging one line naming how
// many candidates it could not read — whenever collectOtherCASMarks reports
// the mark set incomplete, rather than running GC/MaybeGC against a mark set
// it knows may be missing a live reference. Reclaim is best-effort and can
// always wait for a quieter moment (the next call, once whatever was locked
// or unreadable this round has cleared); a swept live blob cannot be undone.
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
// wants the outcome directly instead of parsing the log line. Both are zero
// whenever the sweep was skipped, deferred, or failed.
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
	if unreadable := collectOtherCASMarks(casPath, ownPath, marks); unreadable > 0 {
		logf("golance: cas gc: deferred: %d other index database(s) sharing this CAS could not be read within %s; reclaim requires every one of them to be read first",
			unreadable, otherDBOpenTimeout)
		return store.GCStats{}, false
	}

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
// doc), and reports how many candidates it could not fold into that mark
// set with any confidence: one currently held open by some other live
// writer, unreachable within otherDBOpenTimeout, or one whose own CASDir
// value it could not read for any reason other than never having been
// recorded (store.ErrNotFound). A candidate that opened cleanly but records
// no CASDir at all (a database predating this feature) or a different one
// (an unrelated repository's database sharing this same cache directory) is
// legitimately not a contributor to casPath's mark set and does not count
// toward the returned total — only a candidate this call genuinely could
// not read makes the mark set an under-approximation RunCASGC must not
// sweep against (see its own doc for why).
func collectOtherCASMarks(casPath, ownPath string, marks map[uint64]struct{}) (unreadable int) {
	matches, err := filepath.Glob(filepath.Join(cacheBaseDir(), "golance", indexDBGlobPattern))
	if err != nil {
		return 0
	}
	for _, path := range matches {
		if path == ownPath {
			continue
		}
		db, err := store.OpenReadOnlyTimeout(path, otherDBOpenTimeout)
		if err != nil {
			unreadable++
			continue
		}
		dir, err := db.CASDir()
		switch {
		case err == nil && dir == casPath:
			_ = db.CollectBlobKeys(marks)
		case err != nil && !errors.Is(err, store.ErrNotFound):
			unreadable++
		}
		_ = db.Close()
	}
	return unreadable
}
