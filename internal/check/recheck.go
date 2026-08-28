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

// runRecheck re-type-checks the package in dir: it lists and filters the
// directory's Go files, parses them, resolves dependencies via
// e.newImporter, and type-checks the result. On success the CheckedPackage
// is cached and, if configured, published via Options.OnResult. ctx.Err()
// is checked before and after both parsing and type-checking so a
// canceled recheck returns promptly without touching the cache.
func (e *Engine) runRecheck(ctx context.Context, dir string) (*CheckedPackage, error) {
	e.mu.Lock()
	pi, ok := e.dirs[dir]
	e.mu.Unlock()
	if !ok {
		var err error
		pi, err = e.resolvePackage(dir)
		if err != nil {
			return nil, err
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	files, err := e.resolveFiles(pi, dir)
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
	astFiles, parseErrs := parseFiles(fset, e.reader, files)

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
		dir:         dir,
		fset:        fset,
		files:       astFiles,
		pkg:         pkg,
		info:        info,
		parseErrs:   parseErrs,
		typeErrs:    typeErrs,
		contentHash: hash,
		builtAt:     time.Now(),
	}

	e.store(dir, cp)

	if e.opts.OnResult != nil {
		e.opts.OnResult(newResult(cp, Diagnostics(cp, e.reader)))
	}

	return cp, nil
}

// resolvePackage determines dir's package by probing SnapshotSource with
// each candidate Go file in dir until one resolves. Used when dir is
// invalidated before any file in it has been resolved via Get or SetFocus.
func (e *Engine) resolvePackage(dir string) (pkgInfo, error) {
	candidates, err := listCandidateFiles(dir)
	if err != nil {
		return pkgInfo{}, err
	}
	for _, path := range candidates {
		pkgPath, d, goFiles, ok := e.snap.PackageForFile(path)
		if !ok || d != dir {
			continue
		}
		pi := pkgInfo{pkgPath: pkgPath, goFiles: goFiles}
		e.mu.Lock()
		e.dirs[dir] = pi
		e.mu.Unlock()
		return pi, nil
	}
	return pkgInfo{}, fmt.Errorf("check: no known package for directory %s", dir)
}

// resolveFiles lists dir's candidate Go files and filters them down to
// those declaring pi's package: the non-test files SnapshotSource already
// knows about, plus any _test.go file in the same package (the external
// "_test"-suffixed test package is not supported).
func (e *Engine) resolveFiles(pi pkgInfo, dir string) ([]string, error) {
	candidates, err := listCandidateFiles(dir)
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

// canonicalPackageName determines the Go package name declared in dir,
// preferring pi's known non-test files (which cannot be the external
// "_test" package) and falling back to the first non-"_test" package
// clause found among candidates.
func (e *Engine) canonicalPackageName(pi pkgInfo, candidates []string) (string, error) {
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

// listCandidateFiles lists dir's regular *.go files, skipping names the Go
// tool itself ignores (leading "." or "_").
func listCandidateFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("check: read dir %s: %w", dir, err)
	}
	var files []string
	for _, ent := range entries {
		if ent.IsDir() || !isCandidateGoFile(ent.Name()) {
			continue
		}
		files = append(files, filepath.Join(dir, ent.Name()))
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
// by position.
func parseFiles(fset *token.FileSet, reader overlay.FileReader, files []string) ([]*ast.File, scanner.ErrorList) {
	var errs scanner.ErrorList
	var out []*ast.File
	for _, path := range files {
		src, err := reader.ReadFile(path)
		if err != nil {
			errs.Add(token.Position{Filename: path}, err.Error())
			continue
		}
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
	return out, errs
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
