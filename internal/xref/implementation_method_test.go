package xref

import (
	"context"
	"testing"
)

const (
	pkgSpeaker     = "example.com/xrefmod/speaker"
	pkgSpeakerImpl = "example.com/xrefmod/speakerimpl"
	pkgSpeakerUse  = "example.com/xrefmod/speakeruse"
	pkgGreeterUse  = "example.com/xrefmod/greeteruse"
)

// wantLoc asserts locs contains exactly one location matching (file, name)'s
// first occurrence, failing with locs' full contents otherwise.
func wantLoc(t *testing.T, locs []Location, file, name string) {
	t.Helper()
	wantLine, wantCol := identOccurrence(t, file, name)
	for _, l := range locs {
		if l.File == file && int(l.Line) == wantLine && int(l.Col) == wantCol {
			return
		}
	}
	t.Errorf("locations %+v missing %s at %s:%d:%d", locs, name, file, wantLine, wantCol)
}

// TestImplementation_InterfaceMethodNameListsImplementerMethods covers
// invoking "Go to Implementation" on an interface METHOD name at its
// declaration inside the interface body (as opposed to the interface's own
// name, which TestImplementation_InterfaceToImplementer already covers).
// Speaker has two implementers, one via a value receiver and one via a
// pointer receiver: both methods must be listed.
func TestImplementation_InterfaceMethodNameListsImplementerMethods(t *testing.T) {
	r, snap := newTestResolver(t)

	speakerFile := goFile(t, snap, pkgSpeaker, "speaker.go")
	line, col := identOccurrence(t, speakerFile, "Speak") // the method name inside the interface body

	locs, err := r.Implementation(context.Background(), speakerFile, line, col)
	if err != nil {
		t.Fatalf("Implementation: %v", err)
	}
	if len(locs) != 2 {
		t.Fatalf("Implementation returned %d locations, want 2: %+v", len(locs), locs)
	}

	implFile := goFile(t, snap, pkgSpeakerImpl, "speakerimpl.go")
	wantLoc(t, locs, implFile, "Speak") // ValSpeaker.Speak (value receiver)
	// The second "Speak" occurrence in speakerimpl.go is PtrSpeaker's.
	wantSecondLoc(t, locs, implFile, "Speak")
}

// wantSecondLoc is wantLoc's counterpart for a name's second occurrence in
// file, used when a single file declares the same method name twice (once
// per receiver type in this fixture).
func wantSecondLoc(t *testing.T, locs []Location, file, name string) {
	t.Helper()
	positions := identOccurrences(t, file, name)
	if len(positions) < 2 {
		t.Fatalf("%s: found %d occurrences of %q, want at least 2", file, len(positions), name)
	}
	wantLine, wantCol := positions[1].Line, positions[1].Column
	for _, l := range locs {
		if l.File == file && int(l.Line) == wantLine && int(l.Col) == wantCol {
			return
		}
	}
	t.Errorf("locations %+v missing second %s at %s:%d:%d", locs, name, file, wantLine, wantCol)
}

// TestImplementation_ConcreteMethodNameListsInterfaceMethod covers invoking
// "Go to Implementation" on a concrete type's own method name at its
// declaration, the mirror of the interface-method-name case above: it must
// resolve to the interface method it implements, not fail outright.
func TestImplementation_ConcreteMethodNameListsInterfaceMethod(t *testing.T) {
	r, snap := newTestResolver(t)

	implFile := goFile(t, snap, pkgSpeakerImpl, "speakerimpl.go")
	// ValSpeaker's Speak is the first "Speak" occurrence in this file.
	positions := identOccurrences(t, implFile, "Speak")
	line, col := positions[0].Line, positions[0].Column

	locs, err := r.Implementation(context.Background(), implFile, line, col)
	if err != nil {
		t.Fatalf("Implementation: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation returned %d locations, want 1: %+v", len(locs), locs)
	}

	speakerFile := goFile(t, snap, pkgSpeaker, "speaker.go")
	wantLoc(t, locs, speakerFile, "Speak")
}

// TestImplementation_ConcreteMethodCallListsInterfaceMethod covers invoking
// "Go to Implementation" on a call site through a concretely-typed value
// (as opposed to the method's own declaration above), confirming the fix
// also applies to info.Uses-resolved method references, not just info.Defs.
func TestImplementation_ConcreteMethodCallListsInterfaceMethod(t *testing.T) {
	r, snap := newTestResolver(t)

	useFile := goFile(t, snap, pkgSpeakerUse, "speakeruse.go")
	positions := identOccurrences(t, useFile, "Speak")
	if len(positions) < 2 {
		t.Fatalf("%s: found %d occurrences of Speak, want at least 2 (CallInterface, CallConcrete)", useFile, len(positions))
	}
	// CallConcrete's "speakerimpl.ValSpeaker{}.Speak()" is the second call.
	line, col := positions[1].Line, positions[1].Column

	locs, err := r.Implementation(context.Background(), useFile, line, col)
	if err != nil {
		t.Fatalf("Implementation: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation returned %d locations, want 1: %+v", len(locs), locs)
	}

	speakerFile := goFile(t, snap, pkgSpeaker, "speaker.go")
	wantLoc(t, locs, speakerFile, "Speak")
}

// TestImplementation_UseSiteOfInterfaceTypeListsImplementers covers invoking
// "Go to Implementation" on a USE of the interface type elsewhere (a
// var's type, resolved through a reference rather than the interface's own
// declaration), the third cursor position gopls supports alongside the
// interface name and an interface method name.
func TestImplementation_UseSiteOfInterfaceTypeListsImplementers(t *testing.T) {
	r, snap := newTestResolver(t)

	useFile := goFile(t, snap, pkgGreeterUse, "greeteruse.go")
	line, col := identOccurrence(t, useFile, "Greeter") // "iface.Greeter" in "var G iface.Greeter"

	locs, err := r.Implementation(context.Background(), useFile, line, col)
	if err != nil {
		t.Fatalf("Implementation: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation returned %d locations, want 1: %+v", len(locs), locs)
	}

	implFile := goFile(t, snap, pkgImpl, "impl.go")
	wantLoc(t, locs, implFile, "Person")
}
