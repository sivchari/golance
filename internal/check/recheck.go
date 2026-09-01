package check

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sivchari/golance/internal/overlay"
	"github.com/sivchari/golance/internal/typecheck"
)

// runRecheck re-type-checks key's unit: it lists and filters the
// directory's Go files down to the ones declaring key's variant (base or
// external test — see resolveFiles), parses them, resolves dependencies via
// e.newImporter, and type-checks the result. A file declaring the external
// test package imports its base package (if at all) by its ordinary,
// real import path — resolved through the exact same e.newImporter() chain
// as any other cross-package import (export data, not source), same as
// every dependency; nothing here treats it specially. On success the
// CheckedPackage is committed (see Engine.commit) — cached and, if
// configured, published via Options.OnResult, unless a newer-generation
// recheck for key has already committed. ctx.Err() is checked before and
// after both parsing and type-checking so a canceled recheck returns
// promptly without touching the cache.
func (e *Engine) runRecheck(ctx context.Context, key unitKey) (*CheckedPackage, error) {
	e.mu.Lock()
	pi, ok := e.dirs[key]
	e.mu.Unlock()
	if !ok {
		var err error
		pi, err = e.resolvePackage(key)
		if err != nil {
			return nil, err
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	gen := e.nextGen(key)

	files, err := e.resolveFiles(pi, key.dir)
	if err != nil {
		return nil, err
	}
	hash, err := contentHash(e.reader, files)
	if err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	fset := token.NewFileSet()
	astFiles, texts, parseErrs := parseFiles(fset, e.reader, files)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	imp := e.newImporter()
	pkg, info, typeErrs := typecheck.CheckPackage(fset, astFiles, pi.pkgPath, imp)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cp := &CheckedPackage{
		pkgPath:     pi.pkgPath,
		dir:         key.dir,
		fset:        fset,
		files:       astFiles,
		pkg:         pkg,
		info:        info,
		parseErrs:   parseErrs,
		typeErrs:    typeErrs,
		texts:       texts,
		contentHash: hash,
		builtAt:     time.Now(),
	}

	e.commit(key, gen, cp)

	return cp, nil
}

// resolvePackage determines key's package by probing SnapshotSource with
// each candidate Go file in key.dir until one resolves to key itself (its
// directory and variant both match — a directory can have a candidate file
// for each of its (at most two) units, so a probe matching only the
// directory is not enough once key.variant is checked, see unitKeyFor).
// Used when a unit is invalidated before any file in it has been resolved
// via Get or SetFocus.
func (e *Engine) resolvePackage(key unitKey) (pkgInfo, error) {
	candidates, err := e.listCandidateFiles(key.dir)
	if err != nil {
		return pkgInfo{}, err
	}
	for _, path := range candidates {
		pkgPath, d, goFiles, ok := e.snap.PackageForFile(path)
		if !ok || d != key.dir || unitKeyFor(pkgPath, d) != key {
			continue
		}
		pi := pkgInfo{pkgPath: pkgPath, goFiles: goFiles}
		e.mu.Lock()
		e.dirs[key] = pi
		e.mu.Unlock()
		return pi, nil
	}
	return pkgInfo{}, fmt.Errorf("check: no known package for directory %s", key.dir)
}

// resolveFiles lists dir's candidate Go files and filters them down to
// those declaring pi's package: for the base variant, the non-test files
// SnapshotSource already knows about, plus any _test.go file in the same
// package (the external "_test"-suffixed test package is excluded — see
// canonicalPackageName); for the external test variant (recognized from
// pi.pkgPath, see externalTestVariant), the reverse — only the "_test"
// -suffixed package's own files.
func (e *Engine) resolveFiles(pi pkgInfo, dir string) ([]string, error) {
	candidates, err := e.listCandidateFiles(dir)
	if err != nil {
		return nil, err
	}

	name, err := e.canonicalPackageName(pi, candidates)
	if err != nil {
		return nil, err
	}

	files := make([]string, 0, len(candidates))
	for _, path := range candidates {
		if pn, ok := e.packageClauseName(path); ok && pn == name {
			files = append(files, path)
		}
	}
	return files, nil
}

// canonicalPackageName determines the Go package name resolveFiles should
// keep among dir's candidates. For the external test variant it is the
// first "_test"-suffixed package clause found (pi.goFiles is always empty
// for this variant — see GraphSource.PackageForFile — so there is nothing
// to prefer there). For the base variant (the pre-existing behavior,
// unchanged) it is pi's known non-test files' name, preferred over falling
// back to the first non-"_test" package clause found among candidates.
func (e *Engine) canonicalPackageName(pi pkgInfo, candidates []string) (string, error) {
	if _, ok := externalTestVariant(pi.pkgPath); ok {
		for _, f := range candidates {
			if name, ok := e.packageClauseName(f); ok && strings.HasSuffix(name, "_test") {
				return name, nil
			}
		}
		return "", fmt.Errorf("check: no external test package clause found among %v", candidates)
	}
	for _, f := range pi.goFiles {
		if name, ok := e.packageClauseName(f); ok && !strings.HasSuffix(name, "_test") {
			return name, nil
		}
	}
	for _, f := range candidates {
		if name, ok := e.packageClauseName(f); ok && !strings.HasSuffix(name, "_test") {
			return name, nil
		}
	}
	return "", fmt.Errorf("check: no package clause found among %v", candidates)
}

// packageClauseName reads path's package clause without parsing the rest of
// the file. ok is false if the file cannot be read or has no parseable
// package clause.
func (e *Engine) packageClauseName(path string) (string, bool) {
	src, err := e.reader.ReadFile(path)
	if err != nil {
		return "", false
	}
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.PackageClauseOnly)
	if err != nil || f == nil {
		return "", false
	}
	return f.Name.Name, true
}

// dirLister is an optional capability of an Engine's reader: if it can
// report which of its open documents live in a given directory (as
// *overlay.Overlay does), listCandidateFiles includes those alongside
// whatever os.ReadDir finds on disk — so an unsaved new file, which exists
// only in the overlay, is still a candidate for its directory's package.
type dirLister interface {
	OpenFilesInDir(dir string) []string
}

// listCandidateFiles lists dir's regular *.go files, skipping names the Go
// tool itself ignores (leading "." or "_"), unioning disk content with any
// open overlay documents in dir the reader can report (see dirLister).
func (e *Engine) listCandidateFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("check: read dir %s: %w", dir, err)
	}
	seen := make(map[string]bool, len(entries))
	var files []string
	for _, ent := range entries {
		if ent.IsDir() || !isCandidateGoFile(ent.Name()) {
			continue
		}
		path := filepath.Join(dir, ent.Name())
		seen[path] = true
		files = append(files, path)
	}
	if lister, ok := e.reader.(dirLister); ok {
		for _, path := range lister.OpenFilesInDir(dir) {
			if seen[path] || !isCandidateGoFile(filepath.Base(path)) {
				continue
			}
			seen[path] = true
			files = append(files, path)
		}
	}
	sort.Strings(files)
	return files, nil
}

func isCandidateGoFile(name string) bool {
	if !strings.HasSuffix(name, ".go") {
		return false
	}
	return !strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "_")
}

// parseFiles parses each of files (in order) with parser.AllErrors so a
// syntax error in one file still yields a partial AST rather than aborting
// the whole package, collecting every error into a single ErrorList sorted
// by position. It also returns the exact source bytes each file was parsed
// from, keyed by path, so the caller can attach them to the resulting
// CheckedPackage (see CheckedPackage.FileText) instead of leaving callers
// to re-read the overlay afterward.
func parseFiles(fset *token.FileSet, reader overlay.FileReader, files []string) ([]*ast.File, map[string][]byte, scanner.ErrorList) {
	var errs scanner.ErrorList
	var out []*ast.File
	texts := make(map[string][]byte, len(files))
	for _, path := range files {
		src, err := reader.ReadFile(path)
		if err != nil {
			errs.Add(token.Position{Filename: path}, err.Error())
			continue
		}
		texts[path] = src
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.AllErrors)
		if f != nil {
			out = append(out, f)
		}
		if err != nil {
			var list scanner.ErrorList
			if errors.As(err, &list) {
				errs = append(errs, list...)
			} else {
				errs.Add(token.Position{Filename: path}, err.Error())
			}
		}
	}
	errs.Sort()
	return out, texts, errs
}

// contentHash hashes files' content (overlay-aware, via reader) together
// with their paths, so both edits and additions/removals change the hash.
func contentHash(reader overlay.FileReader, files []string) (string, error) {
	h := sha256.New()
	for _, path := range files {
		data, err := reader.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("check: read %s: %w", path, err)
		}
		h.Write([]byte(path))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
