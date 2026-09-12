package xref

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
)

// renameEmbedFixture is the shared source tree for
// TestRename_EmbeddedTypeRenamesPromotedFieldKeys and its gopls-parity
// counterpart: a type (box.Box) embedded both within its own package
// (box.Wrapper) and across a package boundary (container.Container), each
// referenced by a composite-literal key ("Box: ...") and, for the
// cross-package case, a selector expression (c.Box.Value) that resolves
// through the promoted field too. container.Other's "Box" field is an
// unrelated, non-embedded string field sharing the same name, included as
// a near-miss: renaming box.Box must never touch it. Box's own declaration
// deliberately carries no doc comment, so gopls's separate (and, per
// audit-navigation.md finding 2A, still unfixed) convention of also
// rewriting a doc comment that starts with the renamed symbol's own name
// never triggers here — this fixture isolates the promoted-field-key gap
// this test targets from that unrelated, already-documented gap.
//
// boxDeclLine/boxDeclCol pin Box's own declaration position (1-based,
// matching this package's doc.go coordinate system): both tests assert
// identOccurrence agrees, so the fixture and these constants can never
// silently drift apart. The gopls-parity test needs the position as a
// literal (rather than built from a computed string) to keep its
// exec.Command argument list free of gosec's G204 "subprocess launched
// with variable" finding -- unlike a generic CLI-oracle helper driving
// arbitrary case positions, this fixture is fixed, so its position truly
// is a compile-time constant.
const (
	boxDeclLine = 3
	boxDeclCol  = 6
	boxDeclPos  = "box/box.go:3:6"
)

var renameEmbedFixture = map[string]string{
	"go.mod": "module example.com/renameembed\n\ngo 1.23\n",
	"box/box.go": `package box

type Box struct {
	Value int
}

// Wrapper embeds Box within the box package itself.
type Wrapper struct {
	Box
	Label string
}

func NewWrapper() Wrapper {
	return Wrapper{Box: Box{Value: 1}, Label: "w"}
}
`,
	"container/container.go": `package container

import "example.com/renameembed/box"

// Container embeds box.Box across a package boundary.
type Container struct {
	box.Box
	Name string
}

func New() Container {
	return Container{Box: box.Box{Value: 7}, Name: "x"}
}

func Get(c Container) int {
	return c.Box.Value
}

// Other has a field that happens to share Box's name but is not an
// embedding: a plain string field, unrelated to box.Box.
type Other struct {
	Box string
}

func NewOther() Other {
	return Other{Box: "not the type"}
}
`,
}

// writeRenameEmbedFixture writes renameEmbedFixture under dir.
func writeRenameEmbedFixture(t *testing.T, dir string) {
	t.Helper()
	for rel, content := range renameEmbedFixture {
		writeTestFile(t, dir, rel, content)
	}
}

// applyEdits rewrites file's on-disk content by replacing each edit's
// [Col-1, EndCol-1) byte span on its Line (1-based, matching this
// package's doc.go coordinate system) with its NewText, processing a
// line's edits right-to-left so an earlier replacement's length change
// never invalidates a later one's byte offsets on the same line.
func applyEdits(t *testing.T, file string, edits []Edit) {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	lines := splitLinesKeepEnds(string(data))

	byLine := make(map[uint32][]Edit)
	for _, e := range edits {
		byLine[e.Line] = append(byLine[e.Line], e)
	}
	for line, es := range byLine {
		sort.Slice(es, func(i, j int) bool { return es[i].Col > es[j].Col })
		idx := int(line) - 1
		if idx < 0 || idx >= len(lines) {
			t.Fatalf("%s: edit line %d out of range (%d lines)", file, line, len(lines))
		}
		text := lines[idx]
		for _, e := range es {
			start, end := int(e.Col)-1, int(e.EndCol)-1
			if start < 0 || end > len(text) || start > end {
				t.Fatalf("%s:%d: edit col range [%d,%d) out of bounds for line %q", file, line, start, end, text)
			}
			text = text[:start] + e.NewText + text[end:]
		}
		lines[idx] = text
	}

	out := ""
	for _, l := range lines {
		out += l
	}
	if err := os.WriteFile(file, []byte(out), 0o600); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
}

// splitLinesKeepEnds splits s into lines, each retaining its trailing "\n"
// (except possibly the last), so applyEdits can rejoin them without
// reconstructing line-ending bytes itself.
func splitLinesKeepEnds(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// locKey identifies an edit by file/line/col alone, for set comparison
// independent of NewText.
type locKey struct {
	file string
	line uint32
	col  uint32
}

func editLocSet(edits map[string][]Edit) map[locKey]bool {
	out := make(map[locKey]bool)
	for file, es := range edits {
		for _, e := range es {
			out[locKey{file: file, line: e.Line, col: e.Col}] = true
		}
	}
	return out
}

// wantEditAt asserts that got contains (or, if want is false, omits) an
// edit at (file, line, col), converting through toUint32Pos rather than a
// raw uint32(int) cast so an out-of-range position fails the test instead
// of silently wrapping.
func wantEditAt(t *testing.T, got map[locKey]bool, edits map[string][]Edit, file string, line, col int, want bool, what string) {
	t.Helper()
	l, c, err := toUint32Pos(line, col)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	has := got[locKey{file, l, c}]
	if has != want {
		t.Errorf("Rename edit at %s:%d:%d (%s) present=%v, want %v\nedits: %+v", file, line, col, what, has, want, edits)
	}
}

// TestRename_EmbeddedTypeRenamesPromotedFieldKeys covers
// audit-navigation.md finding 2A: renaming a type that is also embedded
// (so its name doubles as a struct's promoted field name) must rewrite
// every composite-literal key and selector expression using that promoted
// name too, in both the defining package and every cross-package embedder
// — otherwise the renamed workspace no longer compiles. It also proves the
// converse: a field that merely shares the renamed type's name, without
// being an embedding, must not be touched.
func TestRename_EmbeddedTypeRenamesPromotedFieldKeys(t *testing.T) {
	dir := t.TempDir()
	writeRenameEmbedFixture(t, dir)

	r, snap := newResolverForDir(t, dir)
	boxFile := goFile(t, snap, "example.com/renameembed/box", "box.go")
	containerFile := goFile(t, snap, "example.com/renameembed/container", "container.go")

	line, col := identOccurrence(t, boxFile, "Box") // the declaration
	if line != boxDeclLine || col != boxDeclCol {
		t.Fatalf("fixture drifted: Box decl now at %d:%d, want %d:%d (update boxDeclLine/boxDeclCol/boxDeclPos)", line, col, boxDeclLine, boxDeclCol)
	}

	edits, err := r.Rename(context.Background(), boxFile, line, col, "RenamedBox")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	got := editLocSet(edits)

	boxOccs := identOccurrences(t, boxFile, "Box")
	// boxOccs[2]: same-package promoted field key, `Wrapper{Box: Box{Value: 1}, ...}`.
	wantEditAt(t, got, edits, boxFile, boxOccs[2].Line, boxOccs[2].Column, true, "same-package promoted field key")

	containerOccs := identOccurrences(t, containerFile, "Box")
	if len(containerOccs) != 6 {
		t.Fatalf("expected 6 occurrences of %q in %s, got %d: %+v", "Box", containerFile, len(containerOccs), containerOccs)
	}
	// containerOccs[2]: cross-package promoted field key, `Container{Box: box.Box{...}, ...}`.
	wantEditAt(t, got, edits, containerFile, containerOccs[2].Line, containerOccs[2].Column, true, "cross-package promoted field key")
	// containerOccs[3]: cross-package selector through the promoted field, `c.Box.Value`.
	wantEditAt(t, got, edits, containerFile, containerOccs[3].Line, containerOccs[3].Column, true, "promoted field selector")
	// containerOccs[4]/[5]: Other.Box is an unrelated field, not an embedding.
	wantEditAt(t, got, edits, containerFile, containerOccs[4].Line, containerOccs[4].Column, false, "Other's unrelated field declaration")
	wantEditAt(t, got, edits, containerFile, containerOccs[5].Line, containerOccs[5].Column, false, "Other's unrelated composite-literal key")

	for file, es := range edits {
		applyEdits(t, file, es)
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./... after applying golance's rename edits: %v\n%s", err, out)
	}
}

// TestRename_EmbeddedTypeMatchesGopls compares golance's rename edit set
// against gopls v0.23.0's own, for the identical fixture
// TestRename_EmbeddedTypeRenamesPromotedFieldKeys exercises: both tools
// rename the same fixture from two independent copies, and every file
// either tool touched must end up byte-identical, since neither tool has
// any other known behavioral gap on this fixture (no doc comment, no
// external test package — see renameEmbedFixture's own doc for why those
// are deliberately excluded).
func TestRename_EmbeddedTypeMatchesGopls(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not found on PATH; skipping golance-vs-gopls parity check")
	}

	golanceDir := filepath.Join(t.TempDir(), "golance")
	goplsDir := filepath.Join(t.TempDir(), "gopls")
	writeRenameEmbedFixture(t, golanceDir)
	writeRenameEmbedFixture(t, goplsDir)

	r, snap := newResolverForDir(t, golanceDir)
	boxFile := goFile(t, snap, "example.com/renameembed/box", "box.go")
	line, col := identOccurrence(t, boxFile, "Box")
	if line != boxDeclLine || col != boxDeclCol {
		t.Fatalf("fixture drifted: Box decl now at %d:%d, want %d:%d (update boxDeclLine/boxDeclCol/boxDeclPos)", line, col, boxDeclLine, boxDeclCol)
	}

	edits, err := r.Rename(context.Background(), boxFile, line, col, "RenamedBox")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	for file, es := range edits {
		applyEdits(t, file, es)
	}

	cacheDir := t.TempDir()
	cmd := exec.Command("gopls", "rename", "-w", boxDeclPos, "RenamedBox")
	cmd.Dir = goplsDir
	cmd.Env = append(os.Environ(),
		"GOCACHE="+filepath.Join(cacheDir, "gocache"),
		"GOPLSCACHE="+filepath.Join(cacheDir, "goplscache"),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gopls rename: %v\n%s", err, out)
	}

	for rel := range renameEmbedFixture {
		if rel == "go.mod" {
			continue
		}
		golanceContent, err := os.ReadFile(filepath.Clean(filepath.Join(golanceDir, rel)))
		if err != nil {
			t.Fatalf("read golance %s: %v", rel, err)
		}
		goplsContent, err := os.ReadFile(filepath.Clean(filepath.Join(goplsDir, rel)))
		if err != nil {
			t.Fatalf("read gopls %s: %v", rel, err)
		}
		if !bytes.Equal(golanceContent, goplsContent) {
			t.Errorf("%s: golance and gopls disagree after rename\ngolance:\n%s\ngopls:\n%s", rel, golanceContent, goplsContent)
		}
	}
}
