package langfeat

// This file regression-tests a fix to a high-severity finding
// (audit-silent-failures.md's H8): loadBuiltinFile used to memoize
// parseBuiltinFile with sync.OnceValue, so a single transient failure (a
// slow/unavailable "go env GOROOT" at startup) was cached for golance's
// entire process lifetime -- hover, definition, and typeDefinition for
// every builtin identifier stayed broken until restart, with no way for a
// later call to retry. loadBuiltinFile now only caches a success
// permanently; a failure is cached for at most builtinRetryInterval,
// after which a later call retries.
//
// package langfeat (not langfeat_test): builtinFileCache is unexported
// package state this test manipulates directly, to simulate a stale
// cached failure without depending on the real toolchain ever actually
// failing.

import (
	"errors"
	"testing"
	"time"
)

// withBuiltinFileCache sets builtinFileCache to (result, have, lastTry) for
// the duration of t, restoring its previous value in a t.Cleanup -- never
// copying the struct itself (it embeds a sync.Mutex), only its individual
// fields, guarded by that same mutex.
func withBuiltinFileCache(t *testing.T, result builtinFileResult, have bool, lastTry time.Time) {
	t.Helper()

	builtinFileCache.mu.Lock()
	savedResult := builtinFileCache.result
	savedHave := builtinFileCache.have
	savedLastTry := builtinFileCache.lastTry
	savedLoggedFailure := builtinFileCache.loggedFailure
	builtinFileCache.result = result
	builtinFileCache.have = have
	builtinFileCache.lastTry = lastTry
	builtinFileCache.mu.Unlock()

	t.Cleanup(func() {
		builtinFileCache.mu.Lock()
		builtinFileCache.result = savedResult
		builtinFileCache.have = savedHave
		builtinFileCache.lastTry = savedLastTry
		builtinFileCache.loggedFailure = savedLoggedFailure
		builtinFileCache.mu.Unlock()
	})
}

// TestLoadBuiltinFile_RetriesStaleFailureInsteadOfStayingPoisoned seeds
// builtinFileCache with a failure old enough to be past
// builtinRetryInterval, and asserts loadBuiltinFile retries (and, against
// this test's real toolchain, succeeds) rather than serving the stale
// cached error forever.
func TestLoadBuiltinFile_RetriesStaleFailureInsteadOfStayingPoisoned(t *testing.T) {
	sentinel := errors.New("langfeat: simulated transient GOROOT failure")
	withBuiltinFileCache(t, builtinFileResult{err: sentinel}, true, time.Now().Add(-2*builtinRetryInterval))

	file, fset, err := loadBuiltinFile()
	if err != nil {
		t.Fatalf("loadBuiltinFile() after the retry interval elapsed = %v, want nil: a stale cached failure must not permanently block later calls", err)
	}
	if file == nil || fset == nil {
		t.Fatalf("loadBuiltinFile() = (%v, %v, nil), want a real parsed builtin.go", file, fset)
	}
}

// TestLoadBuiltinFile_DoesNotRetryFasterThanBuiltinRetryInterval seeds
// builtinFileCache with a failure recorded just now, and asserts
// loadBuiltinFile serves that exact cached error rather than immediately
// re-running parseBuiltinFile -- the retry interval exists precisely so a
// burst of hover/definition/typeDefinition queries during an outage does
// not re-run `go env`/re-parse builtin.go on every keystroke.
func TestLoadBuiltinFile_DoesNotRetryFasterThanBuiltinRetryInterval(t *testing.T) {
	sentinel := errors.New("langfeat: simulated transient GOROOT failure")
	withBuiltinFileCache(t, builtinFileResult{err: sentinel}, true, time.Now())

	_, _, err := loadBuiltinFile()
	if !errors.Is(err, sentinel) {
		t.Fatalf("loadBuiltinFile() immediately after a fresh cached failure = %v, want the still-rate-limited cached error %v", err, sentinel)
	}
}
