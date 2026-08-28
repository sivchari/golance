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
	indexJobs := fs.Int("index-jobs", envInt("GOLANCE_INDEX_JOBS", 0), "index build parallelism (0 = automatic)")
	memLimit := fs.String("mem-limit", os.Getenv("GOLANCE_MEM_LIMIT"), "GOMEMLIMIT for the indexer subprocess (e.g. 1GiB)")
	offline := fs.Bool("offline", envBool("GOLANCE_OFFLINE", false), "forbid module downloads (GOPROXY=off) during graph load and indexing")
	version := fs.Bool("version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "golance: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	if *version {
		fmt.Fprintln(stdout, "golance "+server.Version)
		return 0
	}

	logOut := stderr
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintf(stderr, "golance: open log file: %v\n", err)
			return 1
		}
		defer func() { _ = f.Close() }()
		logOut = f
	}
	logger := log.New(logOut, "", log.LstdFlags)

	rpcServer := rpc.NewServer(rpc.WithLogger(logger))
	server.New(rpcServer, server.Options{
		Logger:    logger,
		IndexJobs: *indexJobs,
		MemLimit:  *memLimit,
		Offline:   *offline,
	})

	if err := rpcServer.Serve(context.Background(), stdin, stdout); err != nil {
		var exitErr *rpc.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.Code
		}
		fmt.Fprintf(logOut, "golance: serve: %v\n", err)
		return 1
	}
	return 0
}

// runIndexer is cmd/golance's indexer-subprocess entry point: it loads the
// import graph for server.EnvRoot, type-checks every workspace package,
// and persists the result to server.EnvDB, reporting build progress as
// "PROGRESS done total" lines on stdout (see internal/server's
// relayIndexProgress, which reads them back from the subprocess it
// launches).
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
		fmt.Fprintf(stderr, "golance: indexer mode requires %s, %s, and %s\n", server.EnvRoot, server.EnvDB, server.EnvCAS)
		return 1
	}

	if path := os.Getenv("GOLANCE_CPUPROFILE"); path != "" {
		f, err := os.Create(path)
		if err != nil {
			fmt.Fprintf(stderr, "golance: indexer: create cpu profile: %v\n", err)
			return 1
		}
		defer func() { _ = f.Close() }()
		if err := pprof.StartCPUProfile(f); err != nil {
			fmt.Fprintf(stderr, "golance: indexer: start cpu profile: %v\n", err)
			return 1
		}
		defer pprof.StopCPUProfile()
	}
	if path := os.Getenv("GOLANCE_MEMPROFILE"); path != "" {
		defer func() {
			f, err := os.Create(path)
			if err != nil {
				fmt.Fprintf(stderr, "golance: indexer: create mem profile: %v\n", err)
				return
			}
			defer func() { _ = f.Close() }()
			if err := pprof.WriteHeapProfile(f); err != nil {
				fmt.Fprintf(stderr, "golance: indexer: write mem profile: %v\n", err)
			}
		}()
	}
	if path := os.Getenv("GOLANCE_MUTEXPROFILE"); path != "" {
		runtime.SetMutexProfileFraction(1)
		defer writeNamedProfile(stderr, "mutex", path)
	}
	if path := os.Getenv("GOLANCE_BLOCKPROFILE"); path != "" {
		runtime.SetBlockProfileRate(1)
		defer writeNamedProfile(stderr, "block", path)
	}

	loadStart := time.Now()
	patterns := []string{"./..."}
	loadOpts := graph.Options{Dir: root, Offline: envBool(server.EnvOffline, false)}
	snap, fromCache, err := loadGraph(loadOpts, patterns, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "golance: indexer: load graph: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "golance: indexer: graph load (cache=%v) took %s for %d packages\n", fromCache, time.Since(loadStart), len(snap.Packages))

	db, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "golance: indexer: open db: %v\n", err)
		return 1
	}
	defer func() { _ = db.Close() }()

	cas, err := store.OpenCAS(casPath)
	if err != nil {
		fmt.Fprintf(stderr, "golance: indexer: open CAS: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if secs := envInt("GOLANCE_INDEX_DEADLINE_SECONDS", 0); secs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(secs)*time.Second)
		defer cancel()
	}

	stats, err := index.Build(ctx, snap, db, cas, index.Options{
		Parallelism:   envInt(server.EnvIndexJobs, 0),
		RelativePaths: server.RelativeIndexPaths(root),
		Progress: func(done, total int) {
			fmt.Fprintf(stdout, "PROGRESS %d %d\n", done, total)
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "golance: indexer: build: %v\n", err)
		return 1
	}
	if stats.Errors > 0 {
		fmt.Fprintf(stderr, "golance: indexer: %d package(s) failed to type-check\n", stats.Errors)
	}

	// Low-frequency GC of unused CAS blobs (see store.CAS.MaybeTrim's doc):
	// the indexer subprocess is already doing bulk I/O, so this is a better
	// place to pay an occasional directory walk than the interactive main
	// process's startup path. Best effort: a failed trim only costs disk
	// space, never correctness.
	if err := cas.MaybeTrim(time.Now()); err != nil {
		fmt.Fprintf(stderr, "golance: indexer: trim CAS: %v\n", err)
	}
	return 0
}

// loadGraph loads the import graph for opts.Dir and patterns, reusing
// internal/graph's on-disk cache (see graph.LoadCache) whenever it is not
// stale (see graph.Stale — go.mod/go.sum/go.work/go.work.sum unchanged
// since the cache was written), instead of unconditionally re-running
// `go list` on every indexer run. This mirrors internal/server's own
// handleInitialize, except synchronously: the indexer is a one-shot batch
// process with no interactive request to answer while a stale cache
// revalidates, so there is nothing to gain from serving a possibly-stale
// snapshot the way the server does.
//
// fromCache reports which path was taken, for the caller's own logging.
func loadGraph(opts graph.Options, patterns []string, stderr io.Writer) (snap *graph.Snapshot, fromCache bool, err error) {
	if !graph.Stale(opts.Dir) {
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
		fmt.Fprintf(stderr, "golance: indexer: save graph cache: %v\n", err)
	}
	return snap, false, nil
}

// writeNamedProfile writes the named runtime/pprof profile (e.g. "mutex",
// "block") to path, reporting any error to stderr.
func writeNamedProfile(stderr io.Writer, name, path string) {
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(stderr, "golance: indexer: create %s profile: %v\n", name, err)
		return
	}
	defer func() { _ = f.Close() }()
	if err := pprof.Lookup(name).WriteTo(f, 0); err != nil {
		fmt.Fprintf(stderr, "golance: indexer: write %s profile: %v\n", name, err)
	}
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
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
