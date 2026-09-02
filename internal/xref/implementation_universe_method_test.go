package xref

import (
	"context"
	"testing"
)

// TestBuild_InterfaceEmbeddingUniverseErrorDoesNotPanic pins the confirmed
// production crash this fix targets directly: indexing ANY interface that
// embeds the builtin error (an extremely common Go pattern, e.g. `type
// WrappedError interface { error; Code() int }`) used to SIGSEGV inside
// internal/index.methodEntrySelf, which called fn.Pkg().Path() unguarded on
// a *types.Func belonging to the predeclared universe scope (error's own
// Error method has Pkg() == nil, since it declares no home package). That
// crash killed the indexer subprocess on every run for any workspace
// containing such an interface — see methodEntrySelf's doc for the fix.
//
// A struct that embeds the builtin error directly (EmbeddedErr below) hits
// the very same unguarded call through a second path — registerMethodSet's
// promoted-method walk, rather than registerInterfaceMethodSet's — so both
// are exercised here instead of just the interface case the original crash
// report's stack trace showed.
func TestBuild_InterfaceEmbeddingUniverseErrorDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/universeerr\n\ngo 1.23\n")
	writeTestFile(t, dir, "werr/werr.go", `package werr

// WrappedError embeds the builtin error interface -- iface.NumMethods()
// flattens error's own Error method into WrappedError's method set, and
// that Error method's *types.Func belongs to the universe scope (Pkg() ==
// nil), not to this package.
type WrappedError interface {
	error
	Code() int
}

// myErr is WrappedError's concrete implementer, declaring its own Error and
// Code methods directly (the idiomatic, overwhelmingly common way to
// implement an error-embedding interface).
type myErr struct {
	msg  string
	code int
}

func (e *myErr) Error() string { return e.msg }
func (e *myErr) Code() int     { return e.code }

func newErr() WrappedError { return &myErr{msg: "boom", code: 1} }

// EmbeddedErr embeds the builtin error interface as a struct field: its
// promoted Error method is, again, the universe's error.Error, this time
// reached through registerMethodSet (a concrete type's own method set)
// rather than registerInterfaceMethodSet -- a second, independent trigger
// of the same unguarded fn.Pkg() call this fix addresses.
type EmbeddedErr struct {
	error
}
`)

	// index.Build must succeed with zero per-package errors: on the
	// pre-fix code this panics inside the Build worker goroutine (a real
	// nil-pointer dereference, not a recoverable test assertion failure),
	// crashing the whole test binary rather than merely failing this one
	// test -- see newResolverForDir, which fails the test via
	// stats.Errors/err if index.Build itself returns cleanly instead.
	r, snap := newResolverForDir(t, dir)
	werrFile := goFile(t, snap, "example.com/universeerr/werr", "werr.go")

	t.Run("implementation_of_interface_finds_implementer", func(t *testing.T) {
		line, col := identOccurrence(t, werrFile, "WrappedError")
		locs, err := r.Implementation(context.Background(), werrFile, line, col)
		if err != nil {
			t.Fatalf("Implementation(WrappedError): %v", err)
		}
		if len(locs) != 1 {
			t.Fatalf("Implementation(WrappedError) = %+v, want exactly 1 result (myErr)", locs)
		}
		wantLoc(t, locs, werrFile, "myErr")
	})

	t.Run("implementation_of_code_method_finds_implementer_method", func(t *testing.T) {
		// "Code" occurs twice in source (the interface method and myErr's
		// own): the interface declaration is the first occurrence.
		positions := identOccurrences(t, werrFile, "Code")
		if len(positions) < 1 {
			t.Fatalf("no occurrences of Code in %s", werrFile)
		}
		line, col := positions[0].Line, positions[0].Column
		locs, err := r.Implementation(context.Background(), werrFile, line, col)
		if err != nil {
			t.Fatalf("Implementation(WrappedError.Code): %v", err)
		}
		if len(locs) != 1 {
			t.Fatalf("Implementation(WrappedError.Code) = %+v, want exactly 1 result (myErr.Code)", locs)
		}
		myErrCodeLine, myErrCodeCol := positions[1].Line, positions[1].Column
		found := false
		for _, l := range locs {
			if l.File == werrFile && int(l.Line) == myErrCodeLine && int(l.Col) == myErrCodeCol {
				found = true
			}
		}
		if !found {
			t.Errorf("locations %+v missing myErr.Code at %s:%d:%d", locs, werrFile, myErrCodeLine, myErrCodeCol)
		}
	})
}
