package index

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"

	"github.com/sivchari/golance/internal/store"
)

// contentHash returns a deterministic hash of goFiles' contents plus
// buildFlagsFP, used as [store.UnitPointer].ContentHash to detect whether a
// package needs rebuilding. readFile is used instead of os.ReadFile so
// Reindex can source the changed package's content through an overlay.
//
// Each file's path is folded into the hash alongside its bytes, so e.g. two
// files swapping content is still detected as a change even though the
// package's total byte content is unchanged. When relative is set, that
// path is made relative to root first (see Options.RelativePaths) — without
// this, the same package checked out identically in two different git
// worktrees would hash to two different values purely because their
// absolute paths differ, defeating Revalidate's whole point of recognizing
// unchanged content across worktrees.
func contentHash(goFiles []string, buildFlagsFP string, readFile func(string) ([]byte, error), root string, relative bool) (uint64, error) {
	files := append([]string(nil), goFiles...)
	sort.Strings(files)

	h := fnv.New64a()
	_, _ = h.Write([]byte(buildFlagsFP))
	_, _ = h.Write([]byte{0})
	for _, f := range files {
		data, err := readFile(f)
		if err != nil {
			return 0, fmt.Errorf("index: read %s for content hash: %w", f, err)
		}
		key := f
		if relative {
			key = relPath(root, f)
		}
		_, _ = h.Write([]byte(key))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(data)
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64(), nil
}

func readFileDisk(path string) ([]byte, error) {
	return os.ReadFile(filepath.Clean(path))
}

// statFiles stats every file in goFiles and returns their (path, size,
// mtime) as [store.FileStat], for later comparison via filesStatMatch. It
// never reads file content. When relative is set, the recorded Path is
// relative to root (see Options.RelativePaths); otherwise it is goFiles'
// path unchanged and root is unused.
func statFiles(goFiles []string, root string, relative bool) ([]store.FileStat, error) {
	out := make([]store.FileStat, len(goFiles))
	for i, f := range goFiles {
		fi, err := os.Stat(f)
		if err != nil {
			return nil, fmt.Errorf("index: stat %s: %w", f, err)
		}
		p := f
		if relative {
			p = relPath(root, f)
		}
		out[i] = store.FileStat{Path: p, Size: fi.Size(), ModTimeNanos: fi.ModTime().UnixNano()}
	}
	return out, nil
}

// filesStatMatch reports whether goFiles' current on-disk (size, mtime)
// exactly match stored, keyed by path. A different file count (a file was
// added or removed) is always a mismatch. It never reads file content.
// relative and root must match whatever statFiles used to produce stored,
// so its (possibly root-relative) Path values can be joined back to the
// same absolute paths goFiles already is in.
func filesStatMatch(goFiles []string, stored []store.FileStat, root string, relative bool) bool {
	if len(goFiles) != len(stored) {
		return false
	}
	byPath := make(map[string]store.FileStat, len(stored))
	for _, fs := range stored {
		p := fs.Path
		if relative {
			p = filepath.Join(root, fs.Path)
		}
		byPath[p] = fs
	}
	for _, f := range goFiles {
		fi, err := os.Stat(f)
		if err != nil {
			return false
		}
		want, ok := byPath[f]
		if !ok || want.Size != fi.Size() || want.ModTimeNanos != fi.ModTime().UnixNano() {
			return false
		}
	}
	return true
}
