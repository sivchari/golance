package langfeat_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/depcheck"
	"github.com/sivchari/golance/internal/depexport"
	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
	"github.com/sivchari/golance/internal/typecheck"
)

// newCheckedPackageWithProvider is newCheckedPackage plus a depcheck.Provider
// over the same import graph — the production equivalent of dp is
// internal/server's workspace.depProvider, and dp is shared with cp's own
// compilation importer (via depexport.Cache, wired here exactly as
// internal/server.ensureDepProvider shares one Provider between navigation
// and dependency compilation), matching production identity: a dependency
// checked once, for either purpose, is reused for the other.
func newCheckedPackageWithProvider(t *testing.T, reader overlay.FileReader, pkgDir, file string) (cp *check.CheckedPackage, path string, dp *depcheck.Provider) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	src := check.NewGraphSource(snap, reader)
	depFset := token.NewFileSet()
	depCache := typecheck.NewCache()
	depMeta := depcheck.NewGraphMetadataSource(snap)
	dp = depcheck.NewProvider(depMeta, depcheck.Options{})
	depExp := depexport.NewCache(nil, depMeta, dp, depexport.Options{})
	imp := func() types.ImporterFrom {
		return typecheck.NewImporter(depFset, nil, depExp, depCache)
	}
	engine := check.New(src, reader, imp, check.Options{})

	path = filepath.Join(root, pkgDir, file)
	cp, err = engine.Get(context.Background(), path)
	if err != nil {
		t.Fatalf("Get(%s): %v", path, err)
	}
	return cp, path, dp
}

// wantDeclPosition returns the (line, column) of name's top-level
// func/type declaring identifier in filename, parsed independently of any
// checker — the ground truth TestDependencyDefinition_Stdlib's exact-column
// assertions check the provider-backed result against, so the expected
// value tracks whatever the installed Go toolchain's real source says
// rather than a hardcoded, version-fragile constant.
func wantDeclPosition(t *testing.T, filename, name string) (line, col int) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	var found *ast.Ident
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.Name == name {
				found = d.Name
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.Name == name {
					found = ts.Name
				}
			}
		}
	}
	if found == nil {
		t.Fatalf("no top-level func/type declaration named %q in %s", name, filename)
	}
	p := fset.Position(found.Pos())
	return p.Line, p.Column
}

func TestDependencyDefinition_Stdlib(t *testing.T) {
	reader := overlay.New()
	cp, path, dp := newCheckedPackageWithProvider(t, reader, "depuse", "depuse.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	tests := []struct {
		name        string
		substr      string
		declName    string // top-level identifier wantDeclPosition looks up
		wantPkgPath string
		wantSuffix  string
	}{
		{name: "strings.Builder", substr: "strings.Builder", declName: "Builder", wantPkgPath: "strings", wantSuffix: filepath.FromSlash("strings/builder.go")},
		{name: "fmt.Sprintf", substr: "fmt.Sprintf", declName: "Sprintf", wantPkgPath: "fmt", wantSuffix: filepath.FromSlash("fmt/print.go")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			offset := mustIndex(t, text, tt.substr) + len(strings.SplitN(tt.substr, ".", 2)[0]) + 1

			got, err := langfeat.DependencyDefinition(context.Background(), cp, dp, path, offset)
			if err != nil {
				t.Fatalf("DependencyDefinition: %v", err)
			}
			if got == nil {
				t.Fatal("DependencyDefinition returned nil, want a result")
			}
			if got.PkgPath != tt.wantPkgPath {
				t.Errorf("PkgPath = %q, want %q", got.PkgPath, tt.wantPkgPath)
			}
			if !strings.HasSuffix(got.Filename, tt.wantSuffix) {
				t.Errorf("Filename = %q, want it to end with %q", got.Filename, tt.wantSuffix)
			}
			if _, err := os.Stat(got.Filename); err != nil {
				t.Errorf("resolved file %s does not exist on disk: %v", got.Filename, err)
			}
			wantLine, wantCol := wantDeclPosition(t, got.Filename, tt.declName)
			if got.Line != wantLine || got.Col != wantCol {
				t.Errorf("position = %d:%d, want %d:%d (from parsing %s directly)", got.Line, got.Col, wantLine, wantCol, got.Filename)
			}
			if got.EndCol != got.Col+len(tt.declName) {
				t.Errorf("EndCol = %d, want %d (Col + len(%q))", got.EndCol, got.Col+len(tt.declName), tt.declName)
			}
		})
	}
}

// TestDependencyDefinition_EmbeddedStdlibField covers "Go to Definition" on
// an embedded struct field naming a standard-library type: the mirror image
// of TestSamePackageDefinition_EmbeddedField for the cross-package/export-
// data path. Before embeddedFieldTarget, ObjectOf(id) returned the implicit
// field var, whose Pkg() is the EMBEDDING package (not bytes) -- so
// DependencyDefinition declined outright (obj.Pkg() == cp.Package()), and
// definitionFallback's SamePackageDefinition call (tried first) resolved
// the same wrong object to itself instead, per
// TestSamePackageDefinition_EmbeddedField's doc.
func TestDependencyDefinition_EmbeddedStdlibField(t *testing.T) {
	reader := overlay.New()
	cp, path, dp := newCheckedPackageWithProvider(t, reader, "embed", "embedstruct_stdlib.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "bytes.Buffer\n") + len("bytes.")

	got, err := langfeat.DependencyDefinition(context.Background(), cp, dp, path, offset)
	if err != nil {
		t.Fatalf("DependencyDefinition: %v", err)
	}
	if got == nil {
		t.Fatal("DependencyDefinition returned nil, want bytes.Buffer's declaration")
	}
	if got.PkgPath != "bytes" {
		t.Errorf("PkgPath = %q, want %q", got.PkgPath, "bytes")
	}
	if !strings.HasSuffix(got.Filename, filepath.FromSlash("bytes/buffer.go")) {
		t.Errorf("Filename = %q, want it to end with bytes/buffer.go", got.Filename)
	}
	wantLine, wantCol := wantDeclPosition(t, got.Filename, "Buffer")
	if got.Line != wantLine || got.Col != wantCol {
		t.Errorf("position = %d:%d, want %d:%d (from parsing %s directly)", got.Line, got.Col, wantLine, wantCol, got.Filename)
	}
}

// TestDependencyDefinition_EmbeddedStdlibInterface covers "Go to
// Definition" on an embedded interface naming a standard-library interface
// (io.Reader): unlike a struct field, an embedded interface element never
// declares an implicit types.Var, so ObjectOf(id) already resolved straight
// to Uses[id] before embeddedFieldTarget existed -- this pins that this
// case needed no fix and still passes through unaffected.
func TestDependencyDefinition_EmbeddedStdlibInterface(t *testing.T) {
	reader := overlay.New()
	cp, path, dp := newCheckedPackageWithProvider(t, reader, "embed", "embediface.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "io.Reader\n") + len("io.")

	got, err := langfeat.DependencyDefinition(context.Background(), cp, dp, path, offset)
	if err != nil {
		t.Fatalf("DependencyDefinition: %v", err)
	}
	if got == nil {
		t.Fatal("DependencyDefinition returned nil, want io.Reader's declaration")
	}
	if got.PkgPath != "io" {
		t.Errorf("PkgPath = %q, want %q", got.PkgPath, "io")
	}
}

func TestDependencyDefinition_SamePackageReturnsNil(t *testing.T) {
	reader := overlay.New()
	cp, path, dp := newCheckedPackageWithProvider(t, reader, "depuse", "depuse.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// UseStdlib's own declaring identifier is in cp's own package: the
	// workspace facts index already has a better answer for this case, so
	// DependencyDefinition should decline rather than offer a
	// substitute.
	offset := mustIndex(t, text, "func UseStdlib") + len("func ")

	got, err := langfeat.DependencyDefinition(context.Background(), cp, dp, path, offset)
	if err != nil {
		t.Fatalf("DependencyDefinition: %v", err)
	}
	if got != nil {
		t.Errorf("DependencyDefinition = %+v, want nil (declared in cp's own package)", got)
	}
}

// TestSamePackageDefinition_ResolvesLocalIdentifier verifies
// SamePackageDefinition's positive case: an identifier declared in cp's
// own package resolves to its exact declaring identifier, using only cp's
// own AST/types.Info/FileSet — the case DependencyDefinition declines (see
// TestDependencyDefinition_SamePackageReturnsNil).
func TestSamePackageDefinition_ResolvesLocalIdentifier(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "hover", "hover.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// DefaultGreeting's initializer references Greeting, declared earlier
	// in the same file.
	offset := mustIndex(t, text, "= Greeting{") + len("= ")

	got, err := langfeat.SamePackageDefinition(cp, path, offset)
	if err != nil {
		t.Fatalf("SamePackageDefinition: %v", err)
	}
	if got == nil {
		t.Fatal("SamePackageDefinition returned nil, want a result")
	}
	if got.File != path {
		t.Errorf("File = %q, want %q", got.File, path)
	}
	wantOffset := mustIndex(t, text, "type Greeting") + len("type ")
	if got.Range.StartOffset != wantOffset {
		t.Errorf("Range.StartOffset = %d, want %d (Greeting's declaring identifier)", got.Range.StartOffset, wantOffset)
	}
}

// TestSamePackageDefinition_CrossPackageReturnsNil verifies
// SamePackageDefinition declines an identifier declared outside cp's own
// package, the mirror image of TestDependencyDefinition_SamePackageReturnsNil.
func TestSamePackageDefinition_CrossPackageReturnsNil(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "depuse", "depuse.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "strings.Builder") + len("strings.")

	got, err := langfeat.SamePackageDefinition(cp, path, offset)
	if err != nil {
		t.Fatalf("SamePackageDefinition: %v", err)
	}
	if got != nil {
		t.Errorf("SamePackageDefinition = %+v, want nil (declared in a different package)", got)
	}
}

// TestSamePackageDefinition_EmbeddedField covers "Go to Definition" invoked
// on an embedded struct field's name, declared in cp's own package: per
// gopls (golang/go#42254), it must jump to the embedded TYPE's own
// declaration. Before embeddedFieldTarget existed, ObjectOf(id) returned
// the implicit field types.Var types.Info.Defs records at the SAME
// position as the identifier itself, so this resolved to itself instead of
// leaving the cursor's current position -- see embeddedFieldTarget's doc.
func TestSamePackageDefinition_EmbeddedField(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "embed", "embed.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "\tBase\n") + 1

	got, err := langfeat.SamePackageDefinition(cp, path, offset)
	if err != nil {
		t.Fatalf("SamePackageDefinition: %v", err)
	}
	if got == nil {
		t.Fatal("SamePackageDefinition returned nil, want Base's declaration")
	}
	wantOffset := mustIndex(t, text, "type Base") + len("type ")
	if got.Range.StartOffset != wantOffset {
		t.Errorf("Range.StartOffset = %d, want %d (Base's declaring identifier, not the embedded field itself)", got.Range.StartOffset, wantOffset)
	}
}

func TestSamePackageDefinition_NoIdentifier(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "hover", "hover.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "\n\n// Greeting")

	got, err := langfeat.SamePackageDefinition(cp, path, offset)
	if err != nil {
		t.Fatalf("SamePackageDefinition: %v", err)
	}
	if got != nil {
		t.Errorf("SamePackageDefinition = %+v, want nil (no identifier at offset)", got)
	}
}

func TestDependencyDefinition_NoIdentifier(t *testing.T) {
	reader := overlay.New()
	cp, path, dp := newCheckedPackageWithProvider(t, reader, "depuse", "depuse.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "\n\n// UseStdlib")

	got, err := langfeat.DependencyDefinition(context.Background(), cp, dp, path, offset)
	if err != nil {
		t.Fatalf("DependencyDefinition: %v", err)
	}
	if got != nil {
		t.Errorf("DependencyDefinition = %+v, want nil (no identifier at offset)", got)
	}
}

// genericDefinitionCase is one row of the matrix TestDependencyDefinition_Generics
// and TestDependencyDefinition_Generics_GoplsParity both drive: substr/skip
// locate the query position in genericuse.go's text (mustIndex(substr) +
// skip, landing on the target identifier), and either topLevel (a top-level
// FuncDecl/TypeSpec name) or memberOf+memberName (a struct field, a method
// with a receiver of memberOf, or an interface method spec) identifies where
// genericdep.go's independent parse expects the result to land.
type genericDefinitionCase struct {
	name       string
	substr     string
	skip       int
	topLevel   string
	memberOf   string
	memberName string
}

// genericDefinitionCases is the acceptance matrix for "Go to Definition"
// into a symbol reached through an INSTANTIATED generic dependency type
// (genericdep.Box[Concrete], mirroring connectrpc.com/connect's
// Request[T]/Response[T].Msg shape that motivated this fix):
//
//   - a struct field on an instantiated generic type (Msg)
//   - a value-receiver and a pointer-receiver method on an instantiated
//     generic type
//   - a field and a method promoted from an embedded instantiated generic
//     struct (Wrapper embeds Box[int])
//   - a generic function instantiated with an explicit and an inferred type
//     argument at the call site
//   - a type parameter's own constraint method call (t.String(), resolving
//     through genericdep.Stringer -- an ordinary interface method, never an
//     instantiation artifact, so this row is also a no-regression check)
//   - the generic type used directly as a variable's type, and via a type
//     alias of a nested instantiation (Box[Inner[int]]) -- both already
//     resolved correctly before this fix (go/types keeps exactly one
//     TypeName per declaration; see depcheck.OriginObject's doc), so these
//     two rows are no-regression checks too.
var genericDefinitionCases = []genericDefinitionCase{
	{name: "field on instantiated generic type", substr: "b.Msg", skip: len("b."), memberOf: "Box", memberName: "Msg"},
	{name: "value receiver method on instantiated generic type", substr: "b.ValueDescribe", skip: len("b."), memberOf: "Box", memberName: "ValueDescribe"},
	{name: "pointer receiver method on instantiated generic type", substr: "b.PointerDescribe", skip: len("b."), memberOf: "Box", memberName: "PointerDescribe"},
	{name: "field promoted from embedded instantiated generic struct", substr: "w.Msg", skip: len("w."), memberOf: "Box", memberName: "Msg"},
	{name: "method promoted from embedded instantiated generic struct", substr: "w.ValueDescribe", skip: len("w."), memberOf: "Box", memberName: "ValueDescribe"},
	{name: "generic function, explicit type argument", substr: "genericdep.Identity[", skip: len("genericdep."), topLevel: "Identity"},
	{name: "generic function, inferred type argument", substr: "genericdep.Identity(", skip: len("genericdep."), topLevel: "Identity"},
	{name: "type parameter's own constraint method call", substr: "return t.String()", skip: len("return t."), memberOf: "Stringer", memberName: "String"},
	{name: "generic type used as a type name", substr: "var TypeNameVar genericdep.", skip: len("var TypeNameVar genericdep."), topLevel: "Box"},
	{name: "type alias of a nested instantiation", substr: "var AliasVar genericdep.", skip: len("var AliasVar genericdep."), topLevel: "BoxOfInner"},
}

// wantMemberDeclPos returns the (line, column) of memberName's declaring
// identifier in filename -- a struct field, a method with a receiver of
// typeName, or an interface method spec -- parsed independently of any
// checker, mirroring wantDeclPosition's ground-truth role for top-level
// declarations.
func wantMemberDeclPos(t *testing.T, filename, typeName, memberName string) (line, col int) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	var found *ast.Ident
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if id := methodDeclIdent(d, typeName, memberName); id != nil {
				found = id
			}
		case *ast.GenDecl:
			if id := typeMemberDeclIdent(d, typeName, memberName); id != nil {
				found = id
			}
		}
	}
	if found == nil {
		t.Fatalf("no field/method %s.%s found in %s", typeName, memberName, filename)
	}
	p := fset.Position(found.Pos())
	return p.Line, p.Column
}

// methodDeclIdent returns memberName's declaring identifier if d is a
// method (value or pointer receiver, generic or not) on typeName.
func methodDeclIdent(d *ast.FuncDecl, typeName, memberName string) *ast.Ident {
	if d.Recv == nil || len(d.Recv.List) != 1 || d.Name.Name != memberName {
		return nil
	}
	if receiverBaseName(d.Recv.List[0].Type) != typeName {
		return nil
	}
	return d.Name
}

// receiverBaseName strips a pointer and any generic type arguments off a
// method receiver's type expression to get the declared type's own name.
func receiverBaseName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	switch rt := expr.(type) {
	case *ast.Ident:
		return rt.Name
	case *ast.IndexExpr:
		if id, ok := rt.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.IndexListExpr:
		if id, ok := rt.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

// typeMemberDeclIdent returns memberName's declaring identifier if d
// declares typeName as a struct with that field or an interface with that
// method spec.
func typeMemberDeclIdent(d *ast.GenDecl, typeName, memberName string) *ast.Ident {
	var found *ast.Ident
	for _, spec := range d.Specs {
		ts, ok := spec.(*ast.TypeSpec)
		if !ok || ts.Name.Name != typeName {
			continue
		}
		switch tt := ts.Type.(type) {
		case *ast.StructType:
			if id := lastNamedIdent(tt.Fields, memberName); id != nil {
				found = id
			}
		case *ast.InterfaceType:
			if id := lastNamedIdent(tt.Methods, memberName); id != nil {
				found = id
			}
		}
	}
	return found
}

// lastNamedIdent returns the last identifier named memberName among
// fields' names (struct fields or interface method specs share this
// *ast.FieldList shape).
func lastNamedIdent(fields *ast.FieldList, memberName string) *ast.Ident {
	var found *ast.Ident
	for _, field := range fields.List {
		for _, name := range field.Names {
			if name.Name == memberName {
				found = name
			}
		}
	}
	return found
}

// TestDependencyDefinition_Generics is the red-then-green regression test
// for the production bug this fix addresses: jump-to-definition into a
// symbol reached through an INSTANTIATED GENERIC dependency type failed
// with "depcheck: could not resolve X in pkg" (resolveObject's error),
// because the field/method go/types synthesizes when instantiating a
// generic type is not identity-equal to its origin declaration -- see
// depcheck.OriginObject's doc. Before the fix, every memberOf-only row here
// failed with exactly that error; the topLevel-only rows (already-correct
// TypeName/generic-function resolution) are included as no-regression
// checks -- see genericDefinitionCases's doc.
func TestDependencyDefinition_Generics(t *testing.T) {
	reader := overlay.New()
	cp, path, dp := newCheckedPackageWithProvider(t, reader, "genericuse", "genericuse.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	for _, tt := range genericDefinitionCases {
		t.Run(tt.name, func(t *testing.T) {
			offset := mustIndex(t, text, tt.substr) + tt.skip

			got, err := langfeat.DependencyDefinition(context.Background(), cp, dp, path, offset)
			if err != nil {
				t.Fatalf("DependencyDefinition: %v", err)
			}
			if got == nil {
				t.Fatal("DependencyDefinition returned nil, want a result")
			}
			const wantPkgPath = "example.com/langfeatmod/genericdep"
			if got.PkgPath != wantPkgPath {
				t.Errorf("PkgPath = %q, want %q", got.PkgPath, wantPkgPath)
			}
			if !strings.HasSuffix(got.Filename, filepath.FromSlash("genericdep/genericdep.go")) {
				t.Errorf("Filename = %q, want it to end with genericdep/genericdep.go", got.Filename)
			}

			var wantLine, wantCol int
			var wantName string
			if tt.topLevel != "" {
				wantLine, wantCol = wantDeclPosition(t, got.Filename, tt.topLevel)
				wantName = tt.topLevel
			} else {
				wantLine, wantCol = wantMemberDeclPos(t, got.Filename, tt.memberOf, tt.memberName)
				wantName = tt.memberName
			}
			if got.Line != wantLine || got.Col != wantCol {
				t.Errorf("position = %d:%d, want %d:%d (from parsing %s directly)", got.Line, got.Col, wantLine, wantCol, got.Filename)
			}
			if got.EndCol != got.Col+len(wantName) {
				t.Errorf("EndCol = %d, want %d (Col + len(%q))", got.EndCol, got.Col+len(wantName), wantName)
			}
		})
	}
}

// lineCol converts a byte offset in text into the 1-based (line, column)
// pair go/token.FileSet.Position and gopls's own "file:line:col" CLI
// argument both use.
func lineCol(text []byte, offset int) (line, col int) {
	line, col = 1, 1
	for i := 0; i < offset && i < len(text); i++ {
		if text[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

// goplsLocationRE matches the leading "file:line:col-endcol:" (or
// "file:line:col:", with no end column) gopls's "definition" subcommand
// prints its result location as.
var goplsLocationRE = regexp.MustCompile(`^(.+):(\d+):(\d+)(?:-\d+)?:`)

// parseGoplsDefinition extracts the (filename, line, col) gopls's
// "definition" subcommand resolved out, from its first output line.
func parseGoplsDefinition(t *testing.T, output string) (filename string, line, col int) {
	t.Helper()
	firstLine, _, _ := strings.Cut(output, "\n")
	m := goplsLocationRE.FindStringSubmatch(firstLine)
	if m == nil {
		t.Fatalf("could not parse gopls definition output line: %q", firstLine)
	}
	line, err := strconv.Atoi(m[2])
	if err != nil {
		t.Fatalf("parse line %q: %v", m[2], err)
	}
	col, err = strconv.Atoi(m[3])
	if err != nil {
		t.Fatalf("parse column %q: %v", m[3], err)
	}
	return filepath.Clean(m[1]), line, col
}

// TestDependencyDefinition_Generics_GoplsParity checks golance's own result
// for every genericDefinitionCases row against gopls v0.23.0's ("definition"
// CLI subcommand) resolution of the identical query -- gopls quality is the
// bar this fix is held to, not merely "returns a result". Skipped if gopls
// is not on PATH (e.g. CI). GOCACHE is redirected to a per-test temp
// directory so gopls's own build graph load has somewhere writable to
// compile against, independent of the ambient environment's GOCACHE.
func TestDependencyDefinition_Generics_GoplsParity(t *testing.T) {
	goplsPath, err := exec.LookPath("gopls")
	if err != nil {
		t.Skip("gopls not on PATH")
	}

	reader := overlay.New()
	cp, path, dp := newCheckedPackageWithProvider(t, reader, "genericuse", "genericuse.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	moduleRoot, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	gocache := t.TempDir()

	for _, tt := range genericDefinitionCases {
		t.Run(tt.name, func(t *testing.T) {
			offset := mustIndex(t, text, tt.substr) + tt.skip

			got, err := langfeat.DependencyDefinition(context.Background(), cp, dp, path, offset)
			if err != nil {
				t.Fatalf("DependencyDefinition: %v", err)
			}
			if got == nil {
				t.Fatal("DependencyDefinition returned nil, want a result")
			}

			line, col := lineCol(text, offset)
			target := "genericuse/genericuse.go:" + strconv.Itoa(line) + ":" + strconv.Itoa(col)
			args := []string{"definition"}
			args = append(args, target)
			cmd := exec.Command(goplsPath, args...)
			cmd.Dir = moduleRoot
			cmd.Env = append(os.Environ(), "GOCACHE="+gocache)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("gopls definition genericuse/genericuse.go:%d:%d: %v\n%s", line, col, err, out)
			}
			wantFilename, wantLine, wantCol := parseGoplsDefinition(t, string(out))

			gotFilename := filepath.Clean(got.Filename)
			if gotFilename != wantFilename {
				t.Errorf("Filename = %q, gopls resolved %q", gotFilename, wantFilename)
			}
			if got.Line != wantLine || got.Col != wantCol {
				t.Errorf("position = %d:%d, gopls resolved %d:%d", got.Line, got.Col, wantLine, wantCol)
			}
		})
	}
}
