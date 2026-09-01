package langfeat_test

import (
	"go/token"
	"go/types"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
	"github.com/sivchari/golance/internal/typecheck"
)

func TestUnimported_LexicalPrefix(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "unimported", "unimported.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "var _ = fm") + len("var _ = fm")

	got, ok := langfeat.Unimported(cp, text, path, offset)
	if !ok {
		t.Fatal("Unimported() ok = false, want true for a bare identifier prefix")
	}
	if got.Selector != "" || got.Prefix != "fm" {
		t.Errorf("Unimported() = %+v, want {Prefix: fm, Selector: \"\"}", got)
	}
}

func TestUnimported_EmptyLexicalPrefix(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "unimported", "unimported.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// The cursor right after "= " (before "fm" is typed): the previous byte
	// is a space, not an identifier rune, so the prefix is empty.
	offset := mustIndex(t, text, "var _ = fm") + len("var _ = ")

	if _, ok := langfeat.Unimported(cp, text, path, offset); ok {
		t.Error("Unimported() ok = true, want false for an empty lexical prefix (nothing to go on)")
	}
}

func TestUnimported_SelectorUnresolvedPackage(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "unimported", "unimported.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "fmt.Sp") + len("fmt.Sp")

	got, ok := langfeat.Unimported(cp, text, path, offset)
	if !ok {
		t.Fatal("Unimported() ok = false, want true for a qualified selector on an unimported package")
	}
	if got.Selector != "fmt" || got.Prefix != "Sp" {
		t.Errorf("Unimported() = %+v, want {Selector: fmt, Prefix: Sp}", got)
	}
}

func TestUnimported_SelectorAlreadyImported(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "unimported", "unimported.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "strings.To") + len("strings.To")

	if _, ok := langfeat.Unimported(cp, text, path, offset); ok {
		t.Error("Unimported() ok = true, want false: strings is already imported, ordinary selector completion handles it")
	}
}

func TestUnimported_SelectorResolvedValue(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "unimported", "unimported.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "return b.v") + len("return b.v")

	if _, ok := langfeat.Unimported(cp, text, path, offset); ok {
		t.Error("Unimported() ok = true, want false: b resolves to a box value, ordinary member completion handles it")
	}
}

func TestUnimportedPackageItems_RanksBelowInScopeAndAddsImport(t *testing.T) {
	reader := overlay.New()
	_, path := newCheckedPackage(t, reader, "unimported", "unimported.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	inScope := []langfeat.CompletionItem{{Label: "fmtVar", SortText: "0fmtVar"}}
	candidates := []langfeat.UnimportedPackageCandidate{{Name: "fmt", ImportPath: "fmt"}}
	unimported := langfeat.UnimportedPackageItems(path, text, "fm", candidates)
	if len(unimported) != 1 {
		t.Fatalf("UnimportedPackageItems() = %+v, want exactly one item", unimported)
	}
	got := unimported[0]
	if got.Label != "fmt" || got.Kind != langfeat.KindPackage {
		t.Errorf("UnimportedPackageItems()[0] = %+v, want Label fmt, Kind KindPackage", got)
	}
	if got.SortText <= inScope[0].SortText {
		t.Errorf("unimported SortText %q sorts before in-scope SortText %q, want it to sort after", got.SortText, inScope[0].SortText)
	}
	if len(got.AdditionalTextEdits) != 1 {
		t.Fatalf("AdditionalTextEdits = %+v, want exactly one edit", got.AdditionalTextEdits)
	}
	assertImportInserted(t, text, got.AdditionalTextEdits[0], `"fmt"`)
}

func TestUnimportedPackageItems_ExistingImportBlock(t *testing.T) {
	reader := overlay.New()
	_, path := newCheckedPackage(t, reader, "unimported", "unimported.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// unimported.go already imports "strings" (see the fixture), so this
	// exercises inserting a second import into an already-present block
	// rather than creating one from scratch.
	candidates := []langfeat.UnimportedPackageCandidate{{Name: "fmt", ImportPath: "fmt"}}
	items := langfeat.UnimportedPackageItems(path, text, "fm", candidates)
	if len(items) != 1 || len(items[0].AdditionalTextEdits) != 1 {
		t.Fatalf("UnimportedPackageItems() = %+v, want exactly one item with one edit", items)
	}
	edit := items[0].AdditionalTextEdits[0]
	assertImportInserted(t, text, edit, `"fmt"`)
	// The import block itself necessarily changes shape (a single "strings"
	// import becomes a grouped "( ... )" block that now also mentions
	// strings), but the edit's range must stay confined to the import
	// declaration — proof this is the minimal-diff import-block edit
	// (organizeImportsEdit), not addImportAction's whole-file replace,
	// which would need to also overlap wherever the client's own primary
	// completion edit lands.
	funcOffset := mustIndex(t, text, "func packagePrefixSite")
	if edit.Range.EndOffset > funcOffset {
		t.Errorf("edit range %+v extends past the import block (func starts at %d): not a minimal diff", edit.Range, funcOffset)
	}
}

func TestUnimportedMemberItems(t *testing.T) {
	reader := overlay.New()
	_, path := newCheckedPackage(t, reader, "unimported", "unimported.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	fmtPkg := loadFmtPackage(t)
	candidate := langfeat.UnimportedPackageCandidate{Name: "fmt", ImportPath: "fmt"}
	items := langfeat.UnimportedMemberItems(path, text, "Sp", candidate, fmtPkg)
	if !hasLabel(items, "Sprintf") {
		t.Errorf("UnimportedMemberItems() = %+v, want to contain Sprintf", items)
	}
	if hasLabel(items, "Println") {
		t.Errorf("UnimportedMemberItems() = %+v, want Println filtered out (does not match prefix Sp)", items)
	}
	for _, it := range items {
		if len(it.AdditionalTextEdits) != 1 {
			t.Errorf("item %+v has %d AdditionalTextEdits, want exactly 1", it, len(it.AdditionalTextEdits))
		}
	}
}

// assertImportInserted checks that edit, applied to text[edit.Range.
// StartOffset:edit.Range.EndOffset], inserts wantImportLit (a quoted import
// path literal, e.g. `"fmt"`) somewhere in the replaced span — a coarse
// but sufficient check that the edit actually adds the intended import,
// without depending on exact formatting whitespace.
func assertImportInserted(t *testing.T, text []byte, edit langfeat.Edit, wantImportLit string) {
	t.Helper()
	if edit.Range.StartOffset < 0 || edit.Range.EndOffset > len(text) || edit.Range.StartOffset > edit.Range.EndOffset {
		t.Fatalf("edit range %+v out of bounds for text of length %d", edit.Range, len(text))
	}
	if !strings.Contains(edit.NewText, wantImportLit) {
		t.Errorf("edit NewText = %q, want it to contain %s", edit.NewText, wantImportLit)
	}
}

// loadFmtPackage decodes the standard library "fmt" package's export data
// through the same typecheck.Importer/graph.Snapshot machinery
// internal/server's depCacheHolder uses (graph.Snapshot satisfies
// typecheck.ExportFileSource), standing in for the *types.Package a real
// workspace snapshot's on-demand export-data decode would hand
// UnimportedMemberItems.
func loadFmtPackage(t *testing.T) *types.Package {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "fmt")
	if err != nil {
		t.Fatalf("graph.Load(fmt): %v", err)
	}
	imp := typecheck.NewImporter(token.NewFileSet(), nil, snap, typecheck.NewCache())
	pkg, err := imp.ImportFrom("fmt", "", 0)
	if err != nil {
		t.Fatalf("ImportFrom(fmt): %v", err)
	}
	return pkg
}
