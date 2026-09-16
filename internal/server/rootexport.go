package server

import (
	"context"
	"crypto/sha256"

	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/overlay"
	"github.com/sivchari/golance/internal/store"
)

// rootExportSource resolves a ROOT (workspace) package's export data
// straight from the facts index's own persisted blob — the same
// store.UnitBlob.Export internal/index.Build/Reindex already write for
// every root package they type-check, keyed by store.UnitPointer.BlobKey —
// instead of a full from-source check, whenever that blob is still known
// current for pkgPath's exact present state. This is what lets a warm
// cross-package query decode a root import in one CAS read instead of
// engineImporter's previous only option, a full check.Engine.GetPackage
// recheck of pkgPath's own whole transitive closure on every cold cache
// entry.
//
// A blob is only ever reused when both of the following hold; otherwise
// blob reports ok=false and the caller (rootAwareImporter) falls back to
// check.Engine.GetPackage, which is always correct (it resolves through the
// same overlay-aware, content-hash-validated cache every other package
// query already uses):
//
//   - none of pkgPath's files are currently open with unsaved edits (see
//     dirty) — the index is always built from saved (on-disk) content, so an
//     open, dirty file's persisted blob reflects a version the editor buffer
//     has already moved past;
//   - index.PackageChanged reports pkgPath's recorded [store.UnitPointer]
//     still matches its current on-disk content AND every direct
//     dependency's current export hash — the same combined-key comparison
//     Revalidate itself uses, so a dependency edited (and reindexed)
//     elsewhere in the workspace correctly invalidates pkgPath's own stale
//     blob too, not just an edit to pkgPath's own files.
type rootExportSource struct {
	graphSrc *check.GraphSource
	overlay  *overlay.Overlay
	idx      func() *indexState
	relative bool
}

// newRootExportSource returns a rootExportSource resolving against
// graphSrc's current (retargetable) snapshot, ov's live overlay state, and
// whatever *indexState idx currently reports — all three read fresh on
// every blob call, so a rootExportSource built once (at workspace
// construction, see setWorkspace) stays correct across every later
// setWorkspace reuse (graphSrc is retargeted in place, see
// check.GraphSource.Retarget) and every index (re)open (idx reads
// s.idx.Load() live), without needing any retargeting of its own.
func newRootExportSource(graphSrc *check.GraphSource, ov *overlay.Overlay, idx func() *indexState, relative bool) *rootExportSource {
	return &rootExportSource{graphSrc: graphSrc, overlay: ov, idx: idx, relative: relative}
}

// blob resolves pkgPath's persisted export data. ok is false whenever
// reusing it is not safe right now — see the type doc for the exact
// conditions — or whenever the index has nothing recorded for pkgPath at
// all (e.g. it was only just added to the workspace and not yet indexed).
func (r *rootExportSource) blob(ctx context.Context, pkgPath string) (data []byte, ok bool) {
	idx := r.idx()
	if idx == nil || idx.db == nil || idx.cas == nil {
		return nil, false
	}
	_, goFiles, pok := r.graphSrc.PackageDir(pkgPath)
	if !pok || len(goFiles) == 0 || r.dirty(goFiles) {
		return nil, false
	}
	changed, err := index.PackageChanged(ctx, r.graphSrc.Snapshot(), idx.db, pkgPath, index.DefaultToolchainFingerprint(), "", r.relative)
	if err != nil || changed {
		return nil, false
	}
	pointer, err := idx.db.GetUnit(ctx, store.Hash(pkgPath))
	if err != nil {
		return nil, false
	}
	blob, hit, err := idx.cas.Get(ctx, pointer.BlobKey)
	if err != nil || !hit {
		return nil, false
	}
	u, err := store.DecodeUnitBlob(blob)
	if err != nil {
		return nil, false
	}
	return u.Export, true
}

// dirty reports whether any of goFiles is currently open in the editor with
// content that has diverged from its saved (on-disk) content — the same
// open-and-diverged check (*Server).dirtyLines applies to a single file,
// applied here across a whole package's files. See the type doc for why any
// dirty file makes pkgPath's persisted export data unsafe to reuse.
func (r *rootExportSource) dirty(goFiles []string) bool {
	for _, f := range goFiles {
		_, _, hash, open := r.overlay.Get(uri.File(f))
		if !open {
			continue
		}
		disk, err := diskReadFile(f)
		if err != nil {
			return true
		}
		if sha256.Sum256(disk) != hash {
			return true
		}
	}
	return false
}
