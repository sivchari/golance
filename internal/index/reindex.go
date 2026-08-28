package index

import (
	"context"
	"errors"
	"fmt"
	"go/token"
	"time"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/typecheck"
)

// FileReader reads a file's content, e.g. from disk or an editor overlay.
type FileReader func(path string) ([]byte, error)

// Reindex re-resolves changedPkg (read through reader, so unsaved editor
// state is honored) via [processUnit], then walks its reverse-dependency
// closure (snap.ClosureUnits) in topological order doing the same. Nothing
// beyond this needs to decide whether to "propagate": each hop
// independently resolves its own combined blob key from its own content
// plus its direct dependencies' current export hashes (see the package
// doc), so a hop whose own inputs did not actually change is a no-op skip —
// exactly the same soundness property Build now shares, and the same
// early-cutoff behavior this function has always had (a body-only edit
// changes changedPkg's own content but not its export data, so nothing
// downstream of it ever needs revisiting; a signature change does, and
// each further hop cuts off the same way in turn).
func Reindex(ctx context.Context, snap *graph.Snapshot, db *store.DB, cas *store.CAS, changedPkg string, reader FileReader, opts Options) (Stats, error) {
	opts = opts.withDefaults()
	start := time.Now()

	if _, ok := snap.Package(changedPkg); !ok {
		return Stats{}, fmt.Errorf("index: reindex: unknown package %s", changedPkg)
	}

	fset := token.NewFileSet()
	keys := newKeyTable(db)
	exp := newCASExportSource(cas, keys)
	imp := typecheck.NewImporter(fset, exp, snap, typecheck.NewCache())

	var stats Stats
	// trustStat=false: reader may be an editor overlay whose content
	// differs from disk while disk's own stat stays untouched (see
	// processUnit's doc).
	if err := reindexOne(fset, imp, exp, db, cas, keys, snap, opts, changedPkg, reader, false, &stats); err != nil {
		stats.Elapsed = time.Since(start)
		return stats, err
	}

	var firstErr error
	for _, path := range orderedReverseClosure(snap, changedPkg) {
		if err := ctx.Err(); err != nil {
			firstErr = errors.Join(firstErr, err)
			break
		}
		// trustStat=true: every closure hop is always read from disk.
		if err := reindexOne(fset, imp, exp, db, cas, keys, snap, opts, path, readFileDisk, true, &stats); err != nil {
			firstErr = errors.Join(firstErr, err)
		}
	}
	stats.Elapsed = time.Since(start)
	return stats, firstErr
}

// orderedReverseClosure returns every package (other than changedPkg
// itself) that transitively imports it, in the snapshot's topological
// order — the order processUnit's dependency-key lookups require.
func orderedReverseClosure(snap *graph.Snapshot, changedPkg string) []string {
	closureSet := make(map[string]bool)
	for _, p := range snap.ClosureUnits(changedPkg) {
		closureSet[p] = true
	}
	var ordered []string
	for _, p := range snap.Order {
		if p != changedPkg && closureSet[p] {
			ordered = append(ordered, p)
		}
	}
	return ordered
}

// reindexOne resolves path via processUnit and persists its outcome (if
// any), updating stats. A processing error counts toward stats.Errors and
// is returned (propagated as part of Reindex's overall error via
// errors.Join); a persist failure for the pointer-only refresh path is
// best-effort, not fatal — see [buildResults.flushPtrsLocked]'s identical
// rationale.
func reindexOne(fset *token.FileSet, imp *typecheck.Importer, exp *casExportSource, db *store.DB, cas *store.CAS, keys *keyTable, snap *graph.Snapshot, opts Options, path string, reader FileReader, trustStat bool, stats *Stats) error {
	outcome, skipped, err := processUnit(fset, imp, exp, snap, db, cas, keys, opts, path, reader, trustStat)
	if err != nil {
		stats.Errors++
		return fmt.Errorf("index: reindex: %s: %w", path, err)
	}
	if skipped {
		stats.Skipped++
	} else {
		stats.Processed++
	}
	if outcome == nil {
		return nil
	}
	if outcome.entry != nil {
		if err := db.PutUnit(*outcome.entry); err != nil {
			stats.Errors++
			return fmt.Errorf("index: reindex: persist %s: %w", path, err)
		}
	}
	if outcome.ptrRefresh != nil {
		_ = db.PutUnitPointersBatch(map[uint64]store.UnitPointer{outcome.pkgHash: *outcome.ptrRefresh})
	}
	return nil
}
