package xref

import (
	"context"
	"testing"

	"golang.org/x/tools/go/types/objectpath"
)

func TestTypeDeclaration_CrossPackage(t *testing.T) {
	r, snap := newTestResolver(t)

	named, err := r.resolveNamed(context.Background(), pkgImpl, "Person")
	if err != nil {
		t.Fatalf("resolveNamed: %v", err)
	}
	objPath, err := objectpath.For(named.Obj())
	if err != nil {
		t.Fatalf("objectpath.For: %v", err)
	}

	loc, ok := r.TypeDeclaration(context.Background(), pkgImpl, string(objPath))
	if !ok {
		t.Fatal("TypeDeclaration: not found")
	}

	implFile := goFile(t, snap, pkgImpl, "impl.go")
	wantLine, wantCol := identOccurrence(t, implFile, "Person")
	if loc.File != implFile || int(loc.Line) != wantLine || int(loc.Col) != wantCol {
		t.Errorf("TypeDeclaration = %+v, want Person at %s:%d:%d", loc, implFile, wantLine, wantCol)
	}
}

func TestTypeDeclaration_NotFound(t *testing.T) {
	r, _ := newTestResolver(t)

	if _, ok := r.TypeDeclaration(context.Background(), pkgImpl, "NoSuchObject"); ok {
		t.Error("TypeDeclaration: want not found for a nonexistent objectpath")
	}
	if _, ok := r.TypeDeclaration(context.Background(), "example.com/xrefmod/nosuchpkg", "Foo"); ok {
		t.Error("TypeDeclaration: want not found for an unindexed package")
	}
}

func TestSymbolDoc_CrossPackage(t *testing.T) {
	r, _ := newTestResolver(t)

	named, err := r.resolveNamed(context.Background(), pkgImpl, "Person")
	if err != nil {
		t.Fatalf("resolveNamed: %v", err)
	}
	objPath, err := objectpath.For(named.Obj())
	if err != nil {
		t.Fatalf("objectpath.For: %v", err)
	}

	doc, ok := r.SymbolDoc(context.Background(), pkgImpl, string(objPath))
	if !ok {
		t.Fatal("SymbolDoc: not found")
	}
	const want = "Person implements Greeter via Greet.\n"
	if doc != want {
		t.Errorf("SymbolDoc = %q, want %q", doc, want)
	}
}

func TestSymbolDoc_NotFound(t *testing.T) {
	r, _ := newTestResolver(t)

	if _, ok := r.SymbolDoc(context.Background(), pkgImpl, "NoSuchObject"); ok {
		t.Error("SymbolDoc: want not found for a nonexistent objectpath")
	}
}
