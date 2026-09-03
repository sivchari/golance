package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"syscall"
	"time"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/server"
	"github.com/sivchari/golance/internal/store"
)

// run is main's testable body: it dispatches to the indexer subprocess
// entry point when server.EnvIndexer is set, otherwise it parses flags and
// serves LSP over stdin/stdout until the client disconnects or sends
// "exit". It returns the process exit code.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if os.Getenv(server.EnvIndexer) == "1" {
		return runIndexer(stdout, stderr)
	}

	fs := flag.NewFlagSet("golance", flag.ContinueOnError)
	fs.SetOutput(stderr)
	logPath := fs.String("log", os.Getenv("GOLANCE_LOG"), "write server logs to this file (default: stderr)")
	indexJobs := fs.Int("index-jobs", envInt("GOLANCE_INDEX_JOBS"), "index build parallelism (0 = automatic)")
	memLimit := fs.String("mem-limit", os.Getenv("GOLANCE_MEM_LIMIT"), "GOMEMLIMIT for the indexer subprocess (e.g. 1GiB)")
	offline := fs.Bool("offline", envBool("GOLANCE_OFFLINE", false), "forbid module downloads (GOPROXY=off) during graph load and indexing")
	watchDebounceMS := fs.Int("watch-debounce-ms", envInt("GOLANCE_WATCH_DEBOUNCE_MS"), "how long to wait for workspace/didChangeWatchedFiles .go events to go quiet before revalidating the workspace (0 = automatic)")
	version := fs.Bool("version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "golance: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	if *version {
		_, _ = fmt.Fprintln(stdout, "golance "+server.Version)
		return 0
	}

	logOut := stderr
	if *logPath != "" {
		f, err := os.OpenFile(filepath.Clean(*logPath), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "golance: open log file: %v\n", err)
			return 1
		}
		defer func() { _ = f.Close() }()
		logOut = f
	}
	logger := log.New(logOut, "", log.LstdFlags)

	rpcServer := rpc.NewServer(rpc.WithLogger(logger))
	srv := server.New(rpcServer, server.Options{
		Logger:        logger,
		IndexJobs:     *indexJobs,
		MemLimit:      *memLimit,
		Offline:       *offline,
		WatchDebounce: time.Duration(*watchDebounceMS) * time.Millisecond,
	})

	// Bound to the process's own signals, the same way runIndexer's own
	// buildIndex does below: canceling this on SIGINT/SIGTERM (or when
	// Serve returns, via rpc.Server.Context's own child context) is what
	// lets background work rpcServer.Go launches — the indexer subprocess,
	// a didSave-triggered reindex — stop instead of outliving this process
	// as an orphan (see internal/server.Server.idxMu's doc).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := rpcServer.Serve(ctx, stdin, stdout)
	srv.Stop()

	if serveErr != nil {
		var exitErr *rpc.ExitError
		if errors.As(serveErr, &exitErr) {
			return exitErr.Code
		}
		_, _ = fmt.Fprintf(logOut, "golance: serve: %v\n", serveErr)
		return 1
	}
	return 0
}

// runIndexer is cmd/golance's indexer-subprocess entry point: it loads the
// import graph for server.EnvRoot, type-checks every workspace package,
// and persists the result to server.EnvDB, reporting build progress as
// "PROGRESS done total" lines followed by one final "STATS ..." summary
// line on stdout (see internal/server's relayIndexProgress, which reads
// them back from the subprocess it launches).
//
// Exit code contract: runIndexer returns non-zero only when
// server.EnvDB's database is not trustworthy as a whole — a failed graph
// load, a failed database open, or index.Build itself returning an error
// (which index.Build reserves for a canceled run or a failed write, see
// its doc). It returns 0 even when stats.Errors is non-zero: a handful of
// packages individually failing to parse or type-check does not make the
// rest of the database (which this run did successfully write) any less
// usable, and internal/server.buildIndex treats a non-zero exit as
// "nothing usable was indexed," discarding this run's progress entirely
// if no prior database exists to fall back to.
func runIndexer(stdout, stderr io.Writer) int {
	root := os.Getenv(server.EnvRoot)
	dbPath := os.Getenv(server.EnvDB)
	casPath := os.Getenv(server.EnvCAS)
	if root == "" || dbPath == "" || casPath == "" {
		_, _ = fmt.Fprintf(stderr, "golance: indexer mode requires %s, %s, and %s\n", server.EnvRoot, server.EnvDB, server.EnvCAS)
		return 1
	}

	stopProfiling, ok := setupProfiling(stderr)
	defer stopProfiling()
	if !ok {
		return 1
	}

	return buildIndex(stdout, stderr, root, dbPath, casPath)
}

// setupProfiling enables the runtime/pprof profiles requested via
// GOLANCE_CPUPROFILE, GOLANCE_MEMPROFILE, GOLANCE_MUTEXPROFILE, and
// GOLANCE_BLOCKPROFILE, and returns a stop function that writes out
// whichever of them were enabled, in reverse order of setup. The caller
// must defer stop() even when ok is false, since a CPU profile may
// already have been started before a later step failed.
func setupProfiling(stderr io.Writer) (stop func(), ok bool) {
	var stops []func()
	stop = func() {
		for i := len(stops) - 1; i >= 0; i-- {
			stops[i]()
		}
	}

	if path := os.Getenv("GOLANCE_CPUPROFILE"); path != "" {
		f, err := os.Create(filepath.Clean(path))
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "golance: indexer: create cpu profile: %v\n", err)
			return stop, false
		}
		stops = append(stops, func() { _ = f.Close() })
		if err := pprof.StartCPUProfile(f); err != nil {
			_, _ = fmt.Fprintf(stderr, "golance: indexer: start cpu profile: %v\n", err)
			return stop, false
		}
		stops = append(stops, pprof.StopCPUProfile)
	}
	if path := os.Getenv("GOLANCE_MEMPROFILE"); path != "" {
		stops = append(stops, func() { writeHeapProfile(stderr, path) })
	}
	if path := os.Getenv("GOLANCE_MUTEXPROFILE"); path != "" {
		runtime.SetMutexProfileFraction(1)
		stops = append(stops, func() { writeNamedProfile(stderr, "mutex", path) })
	}
	if path := os.Getenv("GOLANCE_BLOCKPROFILE"); path != "" {
		runtime.SetBlockProfileRate(1)
		stops = append(stops, func() { writeNamedProfile(stderr, "block", path) })
	}
	return stop, true
}

// writeHeapProfile writes a heap profile to path, reporting any error to
// stderr.
func writeHeapProfile(stderr io.Writer, path string) {
	f, err := os.Create(filepath.Clean(path))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "golance: indexer: create mem profile: %v\n", err)
		return
	}
	defer func() { _ = f.Close() }()
	if err := pprof.WriteHeapProfile(f); err != nil {
		_, _ = fmt.Fprintf(stderr, "golance: indexer: write mem profile: %v\n", err)
	}
}

// buildIndex loads the import graph, opens the on-disk database and CAS,
// and runs index.Build, reporting progress to stdout and errors to
// stderr. It returns runIndexer's exit code.
func buildIndex(stdout, stderr io.Writer, root, dbPath, casPath string) int {
	loadStart := time.Now()
	patterns := []string{"./..."}
	loadOpts := graph.Options{Dir: root, Offline: envBool(server.EnvOffline, false)}
	snap, fromCache, err := loadGraph(loadOpts, patterns, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "golance: indexer: load graph: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "golance: indexer: graph load (cache=%v) took %s for %d packages\n", fromCache, time.Since(loadStart), len(snap.Packages))

	db, err := store.Open(dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "golance: indexer: open db: %v\n", err)
		return 1
	}
	defer func() { _ = db.Close() }()
	schemaRebuild := db.WasRecreated()
	if err := db.PutCASDir(casPath); err != nil {
		_, _ = fmt.Fprintf(stderr, "golance: indexer: record CAS directory: %v\n", err)
	}

	cas, err := store.OpenCAS(casPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "golance: indexer: open CAS: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if secs := envInt("GOLANCE_INDEX_DEADLINE_SECONDS"); secs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(secs)*time.Second)
		defer cancel()
	}

	stats, err := index.Build(ctx, snap, db, cas, index.Options{
		Parallelism:   envInt(server.EnvIndexJobs),
		RelativePaths: server.RelativeIndexPaths(root),
		Progress: func(done, total int) {
			_, _ = fmt.Fprintf(stdout, "PROGRESS %d %d\n", done, total)
		},
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "golance: indexer: build: %v\n", err)
		return 1
	}
	if stats.Errors > 0 {
		_, _ = fmt.Fprintf(stderr, "golance: indexer: %d package(s) failed to type-check\n", stats.Errors)
	}
	// A final summary line, read back by internal/server's
	// relayIndexProgress and folded into the $/progress "end" notification's
	// message: unlike the per-package "PROGRESS done total" lines above,
	// which only count packages resolved (by any means), this distinguishes
	// how many of them required an actual parse/type-check from how many
	// resolved via a CAS hit alone (e.g. switching back to a previously-seen
	// branch) — the fact a caller wanting to confirm "this build avoided
	// rechecking something" should assert on directly rather than on
	// wall-clock time (see index.Stats.TypeChecked's doc).
	_, _ = fmt.Fprintf(stdout, "STATS processed=%d skipped=%d errors=%d typechecked=%d\n", stats.Processed, stats.Skipped, stats.Errors, stats.TypeChecked)

	// Schema-forced rebuild: db was just discarded and recreated empty (see
	// db.WasRecreated's doc), so the file bbolt just finished rewriting is
	// exactly as bloated as the stale one it replaced — bbolt reuses freed
	// pages within a file but never shrinks it (see internal/store's package
	// doc). This is the one point in the whole system where compacting is
	// cheap relative to the work already just done (a full reindex), so pay
	// it here rather than on every ordinary incremental build.
	if schemaRebuild {
		if err := db.Compact(); err != nil {
			_, _ = fmt.Fprintf(stderr, "golance: indexer: compact db: %v\n", err)
		}
	}

	// Mark-and-sweep GC of unreferenced CAS blobs (see store.CAS.GC's doc):
	// the indexer subprocess is already doing bulk I/O and — for the
	// schema-rebuild case — just orphaned a whole generation of blobs, so
	// this is a better place to pay an occasional directory walk than the
	// interactive main process's own startup path (see
	// internal/server.RunCASGC's other caller). force=schemaRebuild bypasses
	// the usual GCInterval throttle for exactly the event that most needs a
	// prompt sweep; every ordinary build still only walks the directory once
	// per GCInterval. Best effort throughout: a failed or skipped GC only
	// costs disk space, never correctness (see (*store.CAS).GC's own safety
	// doc).
	server.RunCASGC(func(format string, args ...any) {
		_, _ = fmt.Fprintf(stderr, format+"\n", args...)
	}, casPath, dbPath, db, schemaRebuild)
	return 0
}

// loadGraph loads the import graph for opts.Dir and patterns, reusing
// internal/graph's on-disk cache (see graph.LoadCache) whenever it is not
// stale (see graph.Stale — go.mod/go.sum/go.work/go.work.sum unchanged
// since the cache was written), instead of unconditionally re-running
// `go list` on every indexer run. This mirrors internal/server's own
// handleInitialize, except synchronously and WITHOUT trusting a shared
// cache (graph.Shared — see its own doc): the indexer is a one-shot batch
// process building the authoritative facts index, with no interactive
// request to answer while a stale or possibly-mismatched cache revalidates
// in the background the way the server tolerates for fast readiness, so
// there is nothing to gain from risking a snapshot that might reflect a
// DIFFERENT worktree's file set (graph.Stale's own doc: mtime alone cannot
// detect that for a cache shared across worktrees) — a `go list` here is
// now cheap regardless, since graph.Load no longer pays the -export
// compilation cost this cache was originally introduced to avoid.
//
// fromCache reports which path was taken, for the caller's own logging.
func loadGraph(opts graph.Options, patterns []string, stderr io.Writer) (snap *graph.Snapshot, fromCache bool, err error) {
	if !graph.Shared(opts.Dir) && !graph.Stale(opts.Dir) {
		if cached, ok := graph.LoadCache(opts.Dir, patterns, opts.BuildFlags); ok {
			return cached, true, nil
		}
	}
	snap, err = graph.Load(opts, patterns...)
	if err != nil {
		return nil, false, err
	}
	if err := graph.SaveCache(opts.Dir, patterns, opts.BuildFlags, snap); err != nil {
		// Best-effort: a failed cache write just means the next indexer run
		// falls back to `go list` again, not a correctness problem.
		_, _ = fmt.Fprintf(stderr, "golance: indexer: save graph cache: %v\n", err)
	}
	return snap, false, nil
}

// writeNamedProfile writes the named runtime/pprof profile (e.g. "mutex",
// "block") to path, reporting any error to stderr.
func writeNamedProfile(stderr io.Writer, name, path string) {
	f, err := os.Create(filepath.Clean(path))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "golance: indexer: create %s profile: %v\n", name, err)
		return
	}
	defer func() { _ = f.Close() }()
	if err := pprof.Lookup(name).WriteTo(f, 0); err != nil {
		_, _ = fmt.Fprintf(stderr, "golance: indexer: write %s profile: %v\n", name, err)
	}
}

func envInt(key string) int {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
