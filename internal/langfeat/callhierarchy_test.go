package langfeat_test

import (
	"context"
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

func TestCallHierarchyFuncAt(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "callhierarchy", "callhierarchy.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	t.Run("method declaration", func(t *testing.T) {
		offset := mustIndex(t, text, "func (c Calc) Add") + len("func (c Calc) ")
		fn, ok := langfeat.CallHierarchyFuncAt(cp, path, offset)
		if !ok {
			t.Fatal("CallHierarchyFuncAt ok = false, want true")
		}
		if fn.Name() != "Add" {
			t.Errorf("Name = %q, want %q", fn.Name(), "Add")
		}
		if fn.Signature().Recv() == nil {
			t.Error("Signature().Recv() = nil, want Calc's receiver")
		}
	})

	t.Run("interface-mediated call site", func(t *testing.T) {
		offset := mustIndex(t, text, "a.Add(1, 2)") + len("a.")
		fn, ok := langfeat.CallHierarchyFuncAt(cp, path, offset)
		if !ok {
			t.Fatal("CallHierarchyFuncAt ok = false, want true")
		}
		if fn.Name() != "Add" {
			t.Errorf("Name = %q, want %q", fn.Name(), "Add")
		}
		if fn.Pkg().Path() == "" {
			t.Error("Pkg() is unexpectedly nil-ish")
		}
	})

	t.Run("call site", func(t *testing.T) {
		offset := mustIndex(t, text, "Describe(sum)")
		fn, ok := langfeat.CallHierarchyFuncAt(cp, path, offset)
		if !ok {
			t.Fatal("CallHierarchyFuncAt ok = false, want true")
		}
		if fn.Name() != "Describe" {
			t.Errorf("Name = %q, want %q", fn.Name(), "Describe")
		}
	})

	t.Run("non-func identifier", func(t *testing.T) {
		offset := mustIndex(t, text, "sum := a.Add(1, 2)")
		if _, ok := langfeat.CallHierarchyFuncAt(cp, path, offset); ok {
			t.Error("CallHierarchyFuncAt ok = true, want false (sum is a var, not a func)")
		}
	})

	t.Run("no identifier", func(t *testing.T) {
		offset := mustIndex(t, text, "\n\n// Adder")
		if _, ok := langfeat.CallHierarchyFuncAt(cp, path, offset); ok {
			t.Error("CallHierarchyFuncAt ok = true, want false (no identifier at offset)")
		}
	})
}

func TestFuncDeclaration_SamePackage(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "callhierarchy", "callhierarchy.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "Describe(sum)")
	fn, ok := langfeat.CallHierarchyFuncAt(cp, path, offset)
	if !ok {
		t.Fatal("CallHierarchyFuncAt ok = false, want true")
	}

	info, err := langfeat.FuncDeclaration(cp, fn)
	if err != nil {
		t.Fatalf("FuncDeclaration: %v", err)
	}
	if info == nil || info.SameFile == "" {
		t.Fatalf("FuncDeclaration = %+v, want a same-package result", info)
	}
	wantOffset := mustIndex(t, text, "func Describe(n int) int {") + len("func ")
	if info.Range.StartOffset != wantOffset {
		t.Errorf("Range.StartOffset = %d, want %d (Describe's own declaration)", info.Range.StartOffset, wantOffset)
	}
}

func TestFuncDeclaration_CrossPackage(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "callhierarchy", "callhierarchy.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "other.Double(n)") + len("other.")
	fn, ok := langfeat.CallHierarchyFuncAt(cp, path, offset)
	if !ok {
		t.Fatal("CallHierarchyFuncAt ok = false, want true")
	}

	info, err := langfeat.FuncDeclaration(cp, fn)
	if err != nil {
		t.Fatalf("FuncDeclaration: %v", err)
	}
	if info == nil {
		t.Fatal("FuncDeclaration returned nil, want a result")
	}
	if info.SameFile != "" {
		t.Errorf("SameFile = %q, want \"\" (a cross-package result)", info.SameFile)
	}
	if info.PkgPath != "example.com/langfeatmod/callhierarchy/other" {
		t.Errorf("PkgPath = %q, want other's import path", info.PkgPath)
	}
	if info.ObjPath != "Double" {
		t.Errorf("ObjPath = %q, want %q", info.ObjPath, "Double")
	}
}

// TestFuncDeclaration_CrossPackage_GenericMethod verifies FuncDeclaration's
// cross-package branch resolves a method reached through an INSTANTIATED
// generic dependency type (a call to genericdep.Box[Concrete].ValueDescribe):
// objectpath.For(fn) previously failed on the synthetic, as-instantiated
// *types.Func go/types creates for a method on an instantiated named type
// (see depcheck.OriginObject's doc), which FuncDeclaration's own doc treats
// as "unreachable via objectpath" and silently degrades to (nil, nil) --
// exactly like a genuinely unreachable function-local type's method, even
// though this one really is reachable. The roundtrip through dp.DeclAt
// proves ObjPath is not merely non-empty but actually resolves back to
// Box's own ValueDescribe declaration.
func TestFuncDeclaration_CrossPackage_GenericMethod(t *testing.T) {
	reader := overlay.New()
	cp, path, dp := newCheckedPackageWithProvider(t, reader, "genericuse", "genericuse.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "b.ValueDescribe") + len("b.")
	fn, ok := langfeat.CallHierarchyFuncAt(cp, path, offset)
	if !ok {
		t.Fatal("CallHierarchyFuncAt ok = false, want true")
	}

	info, err := langfeat.FuncDeclaration(cp, fn)
	if err != nil {
		t.Fatalf("FuncDeclaration: %v", err)
	}
	if info == nil {
		t.Fatal("FuncDeclaration returned nil, want a result (a method reached through an instantiated generic dependency type)")
	}
	if info.SameFile != "" {
		t.Errorf("SameFile = %q, want \"\" (a cross-package result)", info.SameFile)
	}
	const wantPkgPath = "example.com/langfeatmod/genericdep"
	if info.PkgPath != wantPkgPath {
		t.Errorf("PkgPath = %q, want %q", info.PkgPath, wantPkgPath)
	}
	if info.ObjPath == "" {
		t.Fatal("ObjPath is empty, want a resolvable objectpath")
	}

	declID, _, err := dp.DeclAt(context.Background(), info.PkgPath, info.ObjPath)
	if err != nil {
		t.Fatalf("DeclAt(%s, %s): %v", info.PkgPath, info.ObjPath, err)
	}
	if declID.Name != "ValueDescribe" {
		t.Errorf("DeclAt resolved to %q, want %q", declID.Name, "ValueDescribe")
	}
}

func TestFuncDeclaration_InterfaceMethod(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "callhierarchy", "callhierarchy.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "a.Add(1, 2)") + len("a.")
	fn, ok := langfeat.CallHierarchyFuncAt(cp, path, offset)
	if !ok {
		t.Fatal("CallHierarchyFuncAt ok = false, want true")
	}

	info, err := langfeat.FuncDeclaration(cp, fn)
	if err != nil {
		t.Fatalf("FuncDeclaration: %v", err)
	}
	if info == nil || info.SameFile == "" {
		t.Fatalf("FuncDeclaration = %+v, want a same-package result (Adder is declared in this package)", info)
	}
	wantOffset := mustIndex(t, text, "Add(a, b int) int\n}")
	if info.Range.StartOffset != wantOffset {
		t.Errorf("Range.StartOffset = %d, want %d (the interface method's own declaring identifier)", info.Range.StartOffset, wantOffset)
	}
}

func TestOutgoingCalls_AggregatesFromRanges(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "callhierarchy", "callhierarchy.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "func Caller(a Adder) int {") + len("func ")
	fn, ok := langfeat.CallHierarchyFuncAt(cp, path, offset)
	if !ok {
		t.Fatal("CallHierarchyFuncAt ok = false, want true")
	}

	calls, err := langfeat.OutgoingCalls(cp, fn)
	if err != nil {
		t.Fatalf("OutgoingCalls: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("OutgoingCalls returned %d entries, want 2 (Add, Describe): %+v", len(calls), calls)
	}
	if calls[0].Callee.Name() != "Add" {
		t.Errorf("calls[0].Callee.Name() = %q, want %q", calls[0].Callee.Name(), "Add")
	}
	if len(calls[0].FromRanges) != 2 {
		t.Errorf("calls[0].FromRanges has %d entries, want 2 (a.Add called twice)", len(calls[0].FromRanges))
	}
	if calls[1].Callee.Name() != "Describe" {
		t.Errorf("calls[1].Callee.Name() = %q, want %q", calls[1].Callee.Name(), "Describe")
	}
	if len(calls[1].FromRanges) != 1 {
		t.Errorf("calls[1].FromRanges has %d entries, want 1", len(calls[1].FromRanges))
	}
}

func TestOutgoingCalls_StdlibDependencyAndBuiltinFiltering(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "callhierarchy", "callhierarchy.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "func Describe(n int) int {") + len("func ")
	fn, ok := langfeat.CallHierarchyFuncAt(cp, path, offset)
	if !ok {
		t.Fatal("CallHierarchyFuncAt ok = false, want true")
	}

	calls, err := langfeat.OutgoingCalls(cp, fn)
	if err != nil {
		t.Fatalf("OutgoingCalls: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("OutgoingCalls returned %d entries, want 2 (Sprintf, Double; len must be filtered): %+v", len(calls), calls)
	}
	if calls[0].Callee.Name() != "Sprintf" || calls[0].Callee.Pkg().Path() != "fmt" {
		t.Errorf("calls[0].Callee = %s.%s, want fmt.Sprintf", calls[0].Callee.Pkg().Path(), calls[0].Callee.Name())
	}
	if calls[1].Callee.Name() != "Double" || calls[1].Callee.Pkg().Path() != "example.com/langfeatmod/callhierarchy/other" {
		t.Errorf("calls[1].Callee = %s.%s, want other.Double", calls[1].Callee.Pkg().Path(), calls[1].Callee.Name())
	}
	for _, c := range calls {
		if c.Callee.Name() == "len" {
			t.Error("OutgoingCalls included the builtin len, want it filtered")
		}
	}
}

func TestOutgoingCalls_FunctionLiteral(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "callhierarchy", "callhierarchy.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "func WithLiteral() int {") + len("func ")
	fn, ok := langfeat.CallHierarchyFuncAt(cp, path, offset)
	if !ok {
		t.Fatal("CallHierarchyFuncAt ok = false, want true")
	}

	calls, err := langfeat.OutgoingCalls(cp, fn)
	if err != nil {
		t.Fatalf("OutgoingCalls: %v", err)
	}
	if len(calls) != 1 || calls[0].Callee.Name() != "Describe" {
		t.Fatalf("OutgoingCalls = %+v, want a single Describe entry (called from the nested literal)", calls)
	}
}

func TestOutgoingCalls_NoBody(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "callhierarchy", "callhierarchy.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	t.Run("trivial method", func(t *testing.T) {
		offset := mustIndex(t, text, "func (c Calc) Add") + len("func (c Calc) ")
		fn, ok := langfeat.CallHierarchyFuncAt(cp, path, offset)
		if !ok {
			t.Fatal("CallHierarchyFuncAt ok = false, want true")
		}
		calls, err := langfeat.OutgoingCalls(cp, fn)
		if err != nil {
			t.Fatalf("OutgoingCalls: %v", err)
		}
		if len(calls) != 0 {
			t.Errorf("OutgoingCalls = %+v, want none (Add's body has no calls)", calls)
		}
	})

	t.Run("interface method", func(t *testing.T) {
		offset := mustIndex(t, text, "a.Add(1, 2)") + len("a.")
		fn, ok := langfeat.CallHierarchyFuncAt(cp, path, offset)
		if !ok {
			t.Fatal("CallHierarchyFuncAt ok = false, want true")
		}
		calls, err := langfeat.OutgoingCalls(cp, fn)
		if err != nil {
			t.Fatalf("OutgoingCalls: %v", err)
		}
		if len(calls) != 0 {
			t.Errorf("OutgoingCalls = %+v, want none (an interface method has no body)", calls)
		}
	})
}
