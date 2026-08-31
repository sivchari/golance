package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return db
}

func TestDBUnitPointerRoundTrip(t *testing.T) {
	db := openTestDB(t)
	const pkgHash = 12345

	if _, err := db.GetUnit(context.Background(), pkgHash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUnit before Put: err = %v, want ErrNotFound", err)
	}

	want := UnitPointer{
		BlobKey:     7,
		ContentHash: 9,
		Files: []FileStat{
			{Path: "/a.go", Size: 10, ModTimeNanos: 1234},
			{Path: "/b.go", Size: 0, ModTimeNanos: 5678},
		},
	}
	if err := db.PutUnit(&UnitEntry{PkgHash: pkgHash, Pointer: want}); err != nil {
		t.Fatalf("PutUnit() error = %v", err)
	}

	got, err := db.GetUnit(context.Background(), pkgHash)
	if err != nil {
		t.Fatalf("GetUnit() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetUnit() = %+v, want %+v", got, want)
	}
}

func TestDBPutUnitAppliesIndexEntries(t *testing.T) {
	db := openTestDB(t)
	entry := UnitEntry{
		PkgHash: 1,
		Pointer: UnitPointer{BlobKey: 7, ContentHash: 9},
		Index: PackageIndexEntries{
			Names:   []NameEntry{{Name: "Foo", IDHash: 100}},
			Methods: []MethodSymbolEntry{{Name: "String", Entry: MethodEntry{PkgHash: 1, TypeSymbolIDHash: 100}}},
			SymStrs: []SymStrEntry{{IDHash: 100, SymbolID: "pkg#Foo"}},
		},
	}
	if err := db.PutUnit(&entry); err != nil {
		t.Fatalf("PutUnit() error = %v", err)
	}

	names, err := db.LookupNamePrefix(context.Background(), "foo")
	if err != nil || len(names["foo"]) != 1 || names["foo"][0] != 100 {
		t.Errorf("LookupNamePrefix(foo) = %v, %v, want [100], nil", names, err)
	}
	methods, err := db.LookupMethod(context.Background(), "String")
	if err != nil || len(methods) != 1 {
		t.Errorf("LookupMethod(String) = %v, %v, want 1 entry", methods, err)
	}
	strs, err := db.SymbolIDStrings(context.Background(), 100)
	if err != nil || len(strs) != 1 || strs[0] != "pkg#Foo" {
		t.Errorf("SymbolIDStrings(100) = %v, %v, want [pkg#Foo]", strs, err)
	}
}

func TestDBPutUnitsBatch(t *testing.T) {
	db := openTestDB(t)
	entries := []UnitEntry{
		{PkgHash: 1, Pointer: UnitPointer{BlobKey: 10, ContentHash: 1}},
		{PkgHash: 2, Pointer: UnitPointer{BlobKey: 20, ContentHash: 2}},
	}
	if err := db.PutUnitsBatch(entries); err != nil {
		t.Fatalf("PutUnitsBatch() error = %v", err)
	}
	for _, e := range entries {
		got, err := db.GetUnit(context.Background(), e.PkgHash)
		if err != nil || got.BlobKey != e.Pointer.BlobKey {
			t.Errorf("pkgHash %d: GetUnit() = %+v, %v, want BlobKey %d", e.PkgHash, got, err, e.Pointer.BlobKey)
		}
	}
}

func TestDBPutUnitPointersBatchLeavesIndexUntouched(t *testing.T) {
	db := openTestDB(t)
	if err := db.PutUnit(&UnitEntry{
		PkgHash: 1,
		Pointer: UnitPointer{BlobKey: 10, ContentHash: 1},
		Index:   PackageIndexEntries{Names: []NameEntry{{Name: "Foo", IDHash: 100}}},
	}); err != nil {
		t.Fatalf("PutUnit() error = %v", err)
	}

	refreshed := UnitPointer{BlobKey: 10, ContentHash: 1, Files: []FileStat{{Path: "/a.go", Size: 1, ModTimeNanos: 2}}}
	if err := db.PutUnitPointersBatch(map[uint64]UnitPointer{1: refreshed}); err != nil {
		t.Fatalf("PutUnitPointersBatch() error = %v", err)
	}

	got, err := db.GetUnit(context.Background(), 1)
	if err != nil || !reflect.DeepEqual(got, refreshed) {
		t.Errorf("GetUnit() after PutUnitPointersBatch = %+v, %v, want %+v", got, err, refreshed)
	}
	names, err := db.LookupNamePrefix(context.Background(), "foo")
	if err != nil || len(names["foo"]) != 1 {
		t.Errorf("LookupNamePrefix(foo) after PutUnitPointersBatch = %v, %v, want the original index entry untouched", names, err)
	}
}

func TestDBNameIndexPrefixScan(t *testing.T) {
	db := openTestDB(t)
	if err := db.AddNameSymbol("Foo", 1); err != nil {
		t.Fatalf("AddNameSymbol(Foo) error = %v", err)
	}
	if err := db.AddNameSymbol("Foo", 2); err != nil {
		t.Fatalf("AddNameSymbol(Foo, 2) error = %v", err)
	}
	if err := db.AddNameSymbol("FooBar", 3); err != nil {
		t.Fatalf("AddNameSymbol(FooBar) error = %v", err)
	}
	if err := db.AddNameSymbol("Baz", 4); err != nil {
		t.Fatalf("AddNameSymbol(Baz) error = %v", err)
	}

	got, err := db.LookupNamePrefix(context.Background(), "foo")
	if err != nil {
		t.Fatalf("LookupNamePrefix(foo) error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("LookupNamePrefix(foo) returned %d names, want 2: %v", len(got), got)
	}
	if ids := got["foo"]; len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Errorf("LookupNamePrefix(foo)[\"foo\"] = %v, want [1 2]", ids)
	}
	if ids := got["foobar"]; len(ids) != 1 || ids[0] != 3 {
		t.Errorf("LookupNamePrefix(foo)[\"foobar\"] = %v, want [3]", ids)
	}
	if _, ok := got["baz"]; ok {
		t.Errorf("LookupNamePrefix(foo) unexpectedly contains baz")
	}
}

func TestDBMethodIndex(t *testing.T) {
	db := openTestDB(t)
	if err := db.AddMethodSymbol("String", MethodEntry{PkgHash: 1, TypeSymbolIDHash: 10}); err != nil {
		t.Fatalf("AddMethodSymbol() error = %v", err)
	}
	if err := db.AddMethodSymbol("String", MethodEntry{PkgHash: 2, TypeSymbolIDHash: 20}); err != nil {
		t.Fatalf("AddMethodSymbol() error = %v", err)
	}
	// duplicate add must not create a duplicate entry.
	if err := db.AddMethodSymbol("String", MethodEntry{PkgHash: 1, TypeSymbolIDHash: 10}); err != nil {
		t.Fatalf("AddMethodSymbol() dup error = %v", err)
	}

	entries, err := db.LookupMethod(context.Background(), "String")
	if err != nil {
		t.Fatalf("LookupMethod() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("LookupMethod(String) returned %d entries, want 2: %v", len(entries), entries)
	}

	if entries, err := db.LookupMethod(context.Background(), "Unknown"); err != nil || entries != nil {
		t.Errorf("LookupMethod(Unknown) = %v, %v, want nil, nil", entries, err)
	}
}

func TestDBSymbolIDStringCollisionPath(t *testing.T) {
	db := openTestDB(t)
	const collidingHash = 0xDEADBEEF

	// Simulate two distinct SymbolID strings that happen to share a hash
	// (a real 64-bit collision is not feasible to construct in a test, but
	// PutSymbolIDString/VerifySymbolIDString take the hash explicitly, so
	// we can force the same collision scenario directly).
	idA := "pkgA#A"
	idB := "pkgB#B"
	if err := db.PutSymbolIDString(collidingHash, idA); err != nil {
		t.Fatalf("PutSymbolIDString(idA) error = %v", err)
	}
	if err := db.PutSymbolIDString(collidingHash, idB); err != nil {
		t.Fatalf("PutSymbolIDString(idB) error = %v", err)
	}

	for _, id := range []string{idA, idB} {
		ok, err := db.VerifySymbolIDString(collidingHash, id)
		if err != nil {
			t.Fatalf("VerifySymbolIDString(%q) error = %v", id, err)
		}
		if !ok {
			t.Errorf("VerifySymbolIDString(%q) = false, want true", id)
		}
	}

	ok, err := db.VerifySymbolIDString(collidingHash, "pkgC#C")
	if err != nil {
		t.Fatalf("VerifySymbolIDString(unknown) error = %v", err)
	}
	if ok {
		t.Error("VerifySymbolIDString(unknown) = true, want false")
	}

	// re-putting an already-known string must not duplicate it.
	if err := db.PutSymbolIDString(collidingHash, idA); err != nil {
		t.Fatalf("PutSymbolIDString(idA) dup error = %v", err)
	}
}

func TestHashDeterministic(t *testing.T) {
	// Persisted hashes must be stable across process restarts, so pin the
	// exact FNV-1a value rather than comparing two in-process calls.
	if got := Hash("foo"); got != 0xdcb27518fed9d577 {
		t.Errorf("Hash(\"foo\") = %#x, want %#x (FNV-1a)", got, uint64(0xdcb27518fed9d577))
	}
	if Hash("foo") == Hash("bar") {
		t.Error("Hash(\"foo\") == Hash(\"bar\"), want different hashes for different inputs")
	}
}

func TestBuildSymbolID(t *testing.T) {
	if got, want := BuildSymbolID("example.com/pkg", "Foo"), "example.com/pkg#Foo"; got != want {
		t.Errorf("BuildSymbolID() = %q, want %q", got, want)
	}
}

// TestOpen_DiscardsDatabaseMissingSchemaVersion verifies that Open discards
// and recreates a database whose meta bucket exists (so some earlier Open
// has run against it) but does not record the current schemaVersion —
// exactly the shape of a real pre-CAS golance database: it wrote the same
// "unit" bucket encoding UnitPointer still uses today (so a stale record
// decodes without error), but its BlobKey fields only ever resolved against
// that older build's own blob storage, never today's CAS. Opening it as-is
// would silently serve dangling BlobKey pointers for every package not yet
// reprocessed by a fresh build.
func TestOpen_DiscardsDatabaseMissingSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := db.PutUnit(&UnitEntry{PkgHash: 1, Pointer: UnitPointer{BlobKey: 7, ContentHash: 9}}); err != nil {
		t.Fatalf("PutUnit() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if err := stripSchemaVersion(t, path); err != nil {
		t.Fatalf("stripSchemaVersion() error = %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open() on a version-less database error = %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	if _, err := reopened.GetUnit(context.Background(), 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetUnit(1) after reopening a version-less database = %v, want ErrNotFound (stale data must be discarded, not silently served)", err)
	}
}

// TestOpen_SecondOpenOnLockedDatabaseReturnsAnErrorInsteadOfHanging is a
// regression test for the second-editor-window scenario: without
// Options.Timeout, bbolt.Open retries its exclusive flock indefinitely and
// never returns, which used to hang the second session's "initialize"
// request forever. Open must now fail with a clear, wrapped
// bbolt.ErrTimeout well within openTimeout instead of blocking.
func TestOpen_SecondOpenOnLockedDatabaseReturnsAnErrorInsteadOfHanging(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")

	held, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := held.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	start := time.Now()
	_, err = Open(path)
	elapsed := time.Since(start)

	if !errors.Is(err, bolterrors.ErrTimeout) {
		t.Fatalf("second Open() on a locked database error = %v, want an error wrapping bolterrors.ErrTimeout", err)
	}
	if elapsed >= openTimeout+2*time.Second {
		t.Fatalf("second Open() took %s to fail, want it to fail at around openTimeout (%s) instead of hanging", elapsed, openTimeout)
	}
}

// stripSchemaVersion deletes path's schemaVersionKey in place, simulating a
// database written before that key existed.
func stripSchemaVersion(t *testing.T, path string) error {
	t.Helper()
	bdb, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		return err
	}
	defer func() { _ = bdb.Close() }()
	return bdb.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMeta).Delete(schemaVersionKey)
	})
}
