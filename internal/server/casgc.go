package server

import (
	"errors"
	"os"
	"time"

	"github.com/sivchari/golance/internal/store"
)

// otherDBOpenTimeout bounds how long RunCASGC waits to read another index
// database before giving up on including it in this round's mark set (see
// collectOtherCASMarks's doc for what happens to the mark set when it does)
// — the window within which a candidate genuinely held open by another
// live process's writer handle (see store.DB's own doc on bbolt's
// whole-handle-lifetime lock) can be distinguished from one that is simply
// not a usable database (store.IsLocked reports false for the latter,
// which fails fast rather than timing out — see collectOtherCASMarks).
// Short, since GC must never meaningfully delay whatever triggered it
// (server startup or a schema rebuild); waiting longer would not change
// the outcome for a genuinely busy writer, only delay GC for one that will
// still be busy on the next pass anyway.
const otherDBOpenTimeout = 50 * time.Millisecond

// RunCASGC builds the mark set for casPath — the union of every
// UnitPointer.BlobKey recorded in ownDB (this call's own already-open
// database, read directly in process so it is never subject to
// otherDBOpenTimeout) plus every other database store.CASMembers(casPath)
// reports (see collectOtherCASMarks) — then runs [store.CAS.GC]
// (force=true) or [store.CAS.MaybeGC] (force=false, throttled to once per
// [store.GCInterval]) against it, reporting the result via logf: one line,
// in the same key=value style as (*Server).instrument's slow-request line,
// only when a sweep actually ran (never for a MaybeGC call skipped by the
// interval throttle).
//
// The invariant this whole call must uphold: a blob may only be swept when
// every index database sharing casPath was actually read COMPLETELY this
// round — "opened" is not enough. A database this call knows shares casPath
// (per store.CASMembers) but cannot open because another live process
// currently holds it — as opposed to one that is simply not a usable
// database, which can neither contribute marks nor protect anything
// reachable, and self-heals on its owning session's next store.Open (see
// Open's own doc on discard-and-recreate) — makes the mark set an
// under-approximation of what is genuinely still referenced, and so does a
// database (this call's own ownDB included) that opened fine but had at
// least one "unit" record (*store.DB).CollectBlobKeys could not decode: a
// BlobKey named only by that record never reaches marks either way.
// Sweeping against either kind of incomplete mark set risks deleting a
// blob some database's own UnitPointer still names, which nothing ever
// repairs (see collectOtherCASMarks's doc), unlike an ordinary CAS miss.
// RunCASGC therefore skips the sweep entirely — logging one line naming
// how many candidates it deferred for — whenever either ownDB's own
// CollectBlobKeys or collectOtherCASMarks reports the mark set incomplete,
// rather than running GC/MaybeGC against one it knows may be missing a
// live reference. Reclaim is best-effort and can always wait for a
// quieter moment (the next call, once whatever was locked or undecodable
// this round has cleared); a swept live blob cannot be undone.
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
// casPath plus, at most, len(other index databases sharing casPath) short
// bounded opens. logf is typically *log.Logger.Printf (a running server)
// or a thin fmt.Fprintf wrapper (the indexer subprocess — see
// cmd/golance's own caller); either way it is called at most once per
// invocation for RunCASGC's own summary line, plus one additional line per
// candidate collectOtherCASMarks excludes or defers.
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
	skippedOwn, err := ownDB.CollectBlobKeys(marks)
	if err != nil {
		logf("golance: cas gc: collect own blob keys: %v", err)
		return store.GCStats{}, false
	}
	unreadable := collectOtherCASMarks(logf, casPath, ownPath, marks)
	if skippedOwn > 0 {
		logf("golance: cas gc: %d undecodable unit record(s) in this session's own index database; its mark set is incomplete until it is rebuilt",
			skippedOwn)
		unreadable += skippedOwn
	}
	if unreadable > 0 {
		logf("golance: cas gc: deferred: %d database(s) sharing this CAS could not be fully accounted for this round (locked by another live session, or containing undecodable records); reclaim requires them to be read cleanly first",
			unreadable)
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
// database that store.CASMembers(casPath) reports as ever having claimed
// casPath as its own (see (*store.DB).PutCASDir) into marks, skipping
// ownPath (see RunCASGC's doc), and reports how many of those candidates it
// could not fold into that mark set with any confidence: exclusively a
// candidate currently held open by some other live writer process
// (store.IsLocked) and therefore unreachable within otherDBOpenTimeout —
// its mark set contribution may simply not have been read yet, not because
// it is missing.
//
// A candidate that fails to open for any OTHER reason (a corrupt file, one
// truncated by a crash, or one removed since it last recorded membership)
// is excluded from the mark set without counting toward the returned
// total: such a file can neither contribute marks (nothing in it is
// readable) nor protect a blob it cannot even name, and self-heals to an
// empty database on its owning session's next store.Open (see Open's own
// doc on discard-and-recreate) — there is nothing for RunCASGC to wait for
// there, unlike a live writer's lock. A candidate that opened cleanly but
// records no CASDir at all (a database predating this feature) or a
// different one (an unrelated repository's database whose path happened to
// collide, or one that has since switched CAS directories) is likewise not
// a contributor and does not count toward the total.
//
// A candidate that DOES open cleanly and claims casPath, but has at least
// one "unit" record (*store.DB).CollectBlobKeys cannot decode, counts
// toward the total exactly like a locked candidate does: unlike the
// open-failure cases above, this file unambiguously still claims casPath
// and unambiguously still exists — it simply could not be read
// completely, which is exactly the condition RunCASGC's own doc requires
// deferring the whole sweep for (a BlobKey named only by the undecodable
// record would otherwise never make it into marks at all).
//
// logf receives one line for every candidate this call excludes or defers,
// naming the path and the reason, so a stalled reclaim is diagnosable from
// the log alone.
func collectOtherCASMarks(logf func(format string, args ...any), casPath, ownPath string, marks map[uint64]struct{}) (unreadable int) {
	candidates, err := store.CASMembers(casPath)
	if err != nil {
		logf("golance: cas gc: list members of %s: %v", casPath, err)
		return 0
	}
	for _, path := range candidates {
		if path == ownPath {
			continue
		}
		if _, statErr := os.Stat(path); statErr != nil {
			// Recorded membership but no longer on disk: its database was
			// removed since, taking whatever it once referenced with it.
			continue
		}
		db, err := store.OpenReadOnlyTimeout(path, otherDBOpenTimeout)
		if err != nil {
			if store.IsLocked(err) {
				unreadable++
				logf("golance: cas gc: %s locked by another live session; deferring this round: %v", path, err)
			} else {
				logf("golance: cas gc: %s is not a usable index database, excluding it from this round: %v", path, err)
			}
			continue
		}
		dir, err := db.CASDir()
		switch {
		case err == nil && dir == casPath:
			if skipped, collectErr := db.CollectBlobKeys(marks); collectErr != nil || skipped > 0 {
				unreadable++
				if collectErr != nil {
					logf("golance: cas gc: %s: read unit records: %v", path, collectErr)
				} else {
					logf("golance: cas gc: %s: %d undecodable unit record(s); deferring this round", path, skipped)
				}
			}
		case err != nil && !errors.Is(err, store.ErrNotFound):
			unreadable++
			logf("golance: cas gc: %s: read CAS directory: %v", path, err)
		}
		_ = db.Close()
	}
	return unreadable
}
