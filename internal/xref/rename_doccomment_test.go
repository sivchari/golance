package xref

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// renameDocFixture exercises gopls v0.23.0's own doc-comment-rewriting
// convention (see docCommentEdits' doc): every declaration below is
// independent (its own type, or its own receiver type plus method), so
// renaming any one of them in isolation can never touch another's code or
// comments -- the fixture that unrelatedWordCase and every other
// docCommentCases entry share.
var renameDocFixture = map[string]string{
	"go.mod": "module example.com/renamedoc\n\ngo 1.23\n",
	"doccase/doccase.go": `package doccase

// GenericBox holds a value of type T -- the motivating case: a generic
// type's own doc comment, gopls's rename rewrites it exactly like a
// non-generic one.
type GenericBox[T any] struct {
	Value T
}

// LateMention docs don't open with the type's own name: this sentence
// mentions LateMention only here, mid-sentence, not as the first word.
type LateMention struct{}

// MultiOccur is documented across several lines.
//
// MultiOccur is mentioned again here, and once more: MultiOccur.
type MultiOccur struct{}

/* BlockDocType is documented via a block comment mentioning BlockDocType twice: BlockDocType. */
type BlockDocType struct{}

type NoDocType struct{}

// PlainMethodHolder is PlainMethod's receiver.
type PlainMethodHolder struct{}

// PlainMethod does something plain.
func (h PlainMethodHolder) PlainMethod() int { return 0 }

// QualMethodHolder is QualMethod's receiver.
type QualMethodHolder struct{}

// QualMethodHolder.QualMethod does something receiver-qualified.
func (h QualMethodHolder) QualMethod() int { return 0 }

// StarQualMethodHolder is StarQualMethod's receiver.
type StarQualMethodHolder struct{}

// (*StarQualMethodHolder).StarQualMethod does something star-qualified.
func (h *StarQualMethodHolder) StarQualMethod() int { return 0 }

// UnrelatedHelper is unrelated but mentions UnrelatedTarget in passing: an
// UnrelatedTarget-like helper.
type UnrelatedHelper struct{}

// UnrelatedTarget holds a value used by SubUnrelatedTarget and
// UnrelatedTargets elsewhere -- neither of which is a whole-word match.
type UnrelatedTarget struct{}
`,
}

// writeRenameDocFixture writes renameDocFixture under dir.
func writeRenameDocFixture(t *testing.T, dir string) {
	t.Helper()
	for rel, content := range renameDocFixture {
		writeTestFile(t, dir, rel, content)
	}
}

// docCommentCases is the matrix docCommentEdits' own doc promises to match
// gopls on: a comment not starting with the renamed name, a name mentioned
// only on a later line of a multi-line comment, a block comment, no doc
// comment at all, and a method whose doc uses the plain, "Recv.Method", and
// "(*Recv).Method" forms.
var docCommentCases = []struct {
	name    string
	oldName string
	newName string
}{
	{"generic type, doc starts with its own name", "GenericBox", "ContainerBox"},
	{"doc does not start with the renamed name", "LateMention", "EarlyMention"},
	{"name mentioned only on a later line of a multi-line doc", "MultiOccur", "Combined"},
	{"block comment doc", "BlockDocType", "ChunkType"},
	{"no doc comment", "NoDocType", "RenamedDocless"},
	{"method doc starts with the plain method name", "PlainMethod", "PlainMethodRenamed"},
	{"method doc uses the Recv.Method qualified form", "QualMethod", "QualMethodRenamed"},
	{"method doc uses the (*Recv).Method qualified form", "StarQualMethod", "StarQualMethodRenamed"},
	{"unrelated doc word and substrings are never touched", "UnrelatedTarget", "UnrelatedRenamed"},
}

// TestRename_DocComment_MatchesGopls renames each of docCommentCases in its
// own fresh copy of renameDocFixture, from two independent directories, and
// requires golance's and gopls v0.23.0's resulting files to be byte-for-byte
// identical -- the same golance-vs-gopls parity structure
// TestRename_EmbeddedTypeMatchesGopls uses for the embedded-field-promotion
// gap, applied here to the doc-comment gap audit-navigation.md's "Rename/
// generic struct, cross-package fanout" and "Rename/func referenced from
// both test-file kinds" rows already narrowed down to.
func TestRename_DocComment_MatchesGopls(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not found on PATH; skipping golance-vs-gopls parity check")
	}

	for _, tc := range docCommentCases {
		t.Run(tc.name, func(t *testing.T) {
			golanceDir := filepath.Join(t.TempDir(), "golance")
			goplsDir := filepath.Join(t.TempDir(), "gopls")
			writeRenameDocFixture(t, golanceDir)
			writeRenameDocFixture(t, goplsDir)

			r, snap := newResolverForDir(t, golanceDir)
			file := goFile(t, snap, "example.com/renamedoc/doccase", "doccase.go")
			line, col := identOccurrence(t, file, tc.oldName)

			edits, err := r.Rename(context.Background(), file, line, col, tc.newName)
			if err != nil {
				t.Fatalf("Rename: %v", err)
			}
			for f, es := range edits {
				applyEdits(t, f, es)
			}

			cmd := exec.Command("go", "build", "./...")
			cmd.Dir = golanceDir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("go build ./... after applying golance's rename edits: %v\n%s", err, out)
			}

			goplsFile := filepath.Join(goplsDir, "doccase", "doccase.go")
			posArg := goplsPosArg(goplsDir, goplsFile, line, col)
			cacheDir := t.TempDir()
			// Built by appending onto a constant-literal slice, matching
			// runGoplsReferences' own shape: gosec's G204 check only
			// inspects a spread slice argument's ORIGINAL ":=" literal, not
			// values appended afterward, so this (like that existing
			// helper) does not need to fight the checker with an inline
			// dynamic argument.
			args := []string{"rename", "-w"}
			args = append(args, posArg, tc.newName)
			gcmd := exec.Command("gopls", args...)
			gcmd.Dir = goplsDir
			gcmd.Env = append(os.Environ(),
				"GOCACHE="+filepath.Join(cacheDir, "gocache"),
				"GOPLSCACHE="+filepath.Join(cacheDir, "goplscache"),
			)
			if out, err := gcmd.CombinedOutput(); err != nil {
				t.Fatalf("gopls rename: %v\n%s", err, out)
			}

			golanceContent, err := os.ReadFile(filepath.Clean(file))
			if err != nil {
				t.Fatalf("read golance doccase.go: %v", err)
			}
			goplsContent, err := os.ReadFile(filepath.Clean(goplsFile))
			if err != nil {
				t.Fatalf("read gopls doccase.go: %v", err)
			}
			if !bytes.Equal(golanceContent, goplsContent) {
				t.Errorf("golance and gopls disagree after renaming %s -> %s\ngolance:\n%s\ngopls:\n%s", tc.oldName, tc.newName, golanceContent, goplsContent)
			}
		})
	}
}

// TestRename_DocComment_NeverTouchesUnrelatedWord runs without gopls,
// pinning the one property docCommentEdits' own doc calls out explicitly:
// renaming UnrelatedTarget must rewrite only its own declaration's leading
// "UnrelatedTarget holds a value..." word, never UnrelatedHelper's entirely
// separate doc comment (which merely mentions the same text) and never the
// non-whole-word substrings "SubUnrelatedTarget"/"UnrelatedTargets" on
// UnrelatedTarget's own doc's second line.
func TestRename_DocComment_NeverTouchesUnrelatedWord(t *testing.T) {
	dir := t.TempDir()
	writeRenameDocFixture(t, dir)

	r, snap := newResolverForDir(t, dir)
	file := goFile(t, snap, "example.com/renamedoc/doccase", "doccase.go")
	line, col := identOccurrence(t, file, "UnrelatedTarget")

	edits, err := r.Rename(context.Background(), file, line, col, "UnrelatedRenamed")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	for f, es := range edits {
		applyEdits(t, f, es)
	}

	got, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	content := string(got)

	if !strings.Contains(content, "// UnrelatedHelper is unrelated but mentions UnrelatedTarget in passing: an\n// UnrelatedTarget-like helper.") {
		t.Errorf("UnrelatedHelper's own doc comment was modified; got:\n%s", content)
	}
	if !strings.Contains(content, "// UnrelatedRenamed holds a value used by SubUnrelatedTarget and\n// UnrelatedTargets elsewhere -- neither of which is a whole-word match.") {
		t.Errorf("UnrelatedTarget's own doc comment was not rewritten as expected (substring occurrences must survive untouched); got:\n%s", content)
	}
	if strings.Contains(content, "UnrelatedRenamed-like") || strings.Contains(content, "mentions UnrelatedRenamed") {
		t.Errorf("UnrelatedHelper's doc picked up the rename, but it belongs to a different declaration; got:\n%s", content)
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./... after applying golance's rename edits: %v\n%s", err, out)
	}
}
