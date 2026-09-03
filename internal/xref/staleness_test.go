package xref

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/store"
)

// TestResolver_Invalidate_ReflectsReindexedExportData pins the fix for the
// "Go to Implementation alternates between working and not" instability: a
// Resolver's export-data cache (r.cache) is shared across its whole
// lifetime, and gcexportdata.Read (via typecheck.ReadExport) silently
// returns whatever *types.Package is already cached for a path instead of
// decoding freshly read bytes — so once a package has been queried once,
// every later query kept answering from that first decode regardless of
// how many times the underlying facts were reindexed afterward, unless the
// cache entry is explicitly dropped first (see Resolver.Invalidate's doc).
//
// This reproduces exactly that: query Implementation once (priming the
// cache for both iface.Greeter and impl.Person), remove Person's Greet
// method on disk, reindex impl's package, invalidate the reindexed
// closure, and confirm the next query reflects the new export data
// (Person no longer implements Greeter) instead of the stale decode.
func TestResolver_Invalidate_ReflectsReindexedExportData(t *testing.T) {
	root := copyTestModule(t)
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}

	db, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("db.Close: %v", err)
		}
	})
	cas, err := store.OpenCAS(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	if _, err := index.Build(context.Background(), snap, db, cas, &index.Options{}); err != nil {
		t.Fatalf("index.Build: %v", err)
	}

	r := New(db, cas, snap, false)

	ifaceFile := goFile(t, snap, pkgIface, "iface.go")
	line, col := identOccurrence(t, ifaceFile, "Greeter")

	locs, err := r.Implementation(context.Background(), ifaceFile, line, col)
	if err != nil {
		t.Fatalf("baseline Implementation: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("baseline Implementation = %d locations, want 1 (Person implements Greeter): %+v", len(locs), locs)
	}

	// Remove Person's Greet method, and its only caller (user.Use), so
	// Person no longer implements Greeter and the module keeps compiling.
	implFile := goFile(t, snap, pkgImpl, "impl.go")
	newImpl := `package impl

type Person struct {
	Name string
}

func NewPerson(name string) Person {
	return Person{Name: name}
}
`
	if err := os.WriteFile(implFile, []byte(newImpl), 0o600); err != nil {
		t.Fatalf("write %s: %v", implFile, err)
	}
	userFile := goFile(t, snap, pkgUser, "user.go")
	newUser := `package user

import "example.com/xrefmod/impl"

func Declare() impl.Person {
	return impl.Person{Name: "a"}
}
`
	if err := os.WriteFile(userFile, []byte(newUser), 0o600); err != nil {
		t.Fatalf("write %s: %v", userFile, err)
	}

	if _, err := index.Reindex(context.Background(), snap, db, cas, pkgImpl, os.ReadFile, &index.Options{}); err != nil {
		t.Fatalf("index.Reindex: %v", err)
	}
	r.Invalidate(append([]string{pkgImpl}, snap.ClosureUnits(pkgImpl)...))

	locs, err = r.Implementation(context.Background(), ifaceFile, line, col)
	if err != nil {
		t.Fatalf("Implementation after reindex: %v", err)
	}
	if len(locs) != 0 {
		t.Errorf("Implementation after removing Person.Greet and reindexing = %d locations, want 0 (a stale cached decode was reused instead of the reindexed export data): %+v", len(locs), locs)
	}
}
