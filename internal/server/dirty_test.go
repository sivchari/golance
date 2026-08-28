package server

import "testing"

func TestDirtyLineMap(t *testing.T) {
	saved := "package p\n\nfunc A() {}\n\nfunc B() {}\n"
	// dirty inserts two lines before "func B() {}".
	dirty := "package p\n\nfunc A() {}\n\n// new comment\n// another\nfunc B() {}\n"

	tests := []struct {
		name     string
		from, to string
		line     uint32
		want     uint32
		wantOK   bool
	}{
		{"above the edit, unchanged", saved, dirty, 1, 1, true},
		{"still above the edit (blank line)", saved, dirty, 4, 4, true},
		{"at/below the edit, shifted by +2", saved, dirty, 5, 7, true},
		{"reverse direction, shifted by -2", dirty, saved, 7, 5, true},
		{"identical content, no shift", saved, saved, 3, 3, true},
		{"line zero is invalid", saved, dirty, 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := dirtyLineMap([]byte(tt.from), []byte(tt.to), tt.line)
			if ok != tt.wantOK {
				t.Fatalf("dirtyLineMap(line=%d) ok = %v, want %v", tt.line, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Fatalf("dirtyLineMap(line=%d) = %d, want %d", tt.line, got, tt.want)
			}
		})
	}
}

// TestDirtyLineMapDeletion covers the shrink case (dirty has fewer lines
// than saved) and the "shift lands out of range" unmappable case.
func TestDirtyLineMapDeletion(t *testing.T) {
	saved := "package p\n\nfunc A() {}\n\n// comment\n// another\nfunc B() {}\n"
	dirty := "package p\n\nfunc A() {}\n\nfunc B() {}\n"

	// "func B() {}" is saved line 7, dirty line 5 (shift -2).
	got, ok := dirtyLineMap([]byte(saved), []byte(dirty), 7)
	if !ok || got != 5 {
		t.Fatalf("dirtyLineMap(7) = (%d,%v), want (5,true)", got, ok)
	}

	// Mapping a saved line that only existed inside the deleted region
	// (e.g. line 5, "// comment") into dirty content has no sensible
	// target; it must be reported as unmappable, not silently wrong.
	if _, ok := dirtyLineMap([]byte(saved), []byte(dirty), 5); ok {
		t.Fatalf("dirtyLineMap(5): expected ok=false for a line inside the deleted region")
	}
}

func TestDirtyLines(t *testing.T) {
	dir := t.TempDir()
	path := writeTempFile(t, dir, "a.go", "package p\n\nfunc A() {}\n")

	s := &Server{overlay: newTestOverlay()}

	if _, _, ok := s.dirtyLines(path); ok {
		t.Fatalf("dirtyLines: expected ok=false for a file that is not open")
	}

	openDoc(t, s, path, "package p\n\nfunc A() {}\n") // identical to disk
	if _, _, ok := s.dirtyLines(path); ok {
		t.Fatalf("dirtyLines: expected ok=false for an open file with no unsaved changes")
	}

	changeDoc(t, s, path, 2, "package p\n\n// edited\nfunc A() {}\n")
	saved, dirty, ok := s.dirtyLines(path)
	if !ok {
		t.Fatalf("dirtyLines: expected ok=true for a dirty file")
	}
	if string(saved) != "package p\n\nfunc A() {}\n" {
		t.Fatalf("dirtyLines saved = %q", saved)
	}
	if string(dirty) != "package p\n\n// edited\nfunc A() {}\n" {
		t.Fatalf("dirtyLines dirty = %q", dirty)
	}
}
