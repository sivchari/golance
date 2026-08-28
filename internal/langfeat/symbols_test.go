package langfeat_test

import (
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

func findSymbol(syms []langfeat.Symbol, name string) *langfeat.Symbol {
	for i := range syms {
		if syms[i].Name == name {
			return &syms[i]
		}
	}
	return nil
}

func TestDocumentSymbols_Hierarchy(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "symbols", "symbols.go")

	syms, err := langfeat.DocumentSymbols(cp, path)
	if err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}

	widget := findSymbol(syms, "Widget")
	if widget == nil {
		t.Fatal("Widget type symbol not found")
	}
	if widget.Kind != langfeat.SymbolType {
		t.Errorf("Widget.Kind = %v, want SymbolType", widget.Kind)
	}
	describe := findSymbol(widget.Children, "Describe")
	if describe == nil {
		t.Fatal("Widget.Describe method not nested under Widget")
	}
	if describe.Kind != langfeat.SymbolMethod {
		t.Errorf("Describe.Kind = %v, want SymbolMethod", describe.Kind)
	}

	newWidget := findSymbol(syms, "NewWidget")
	if newWidget == nil {
		t.Fatal("NewWidget constructor func not found at top level")
	}
	if newWidget.Kind != langfeat.SymbolFunc {
		t.Errorf("NewWidget.Kind = %v, want SymbolFunc", newWidget.Kind)
	}

	if s := findSymbol(syms, "TopLevel"); s == nil || s.Kind != langfeat.SymbolFunc {
		t.Errorf("TopLevel = %+v, want a top-level SymbolFunc", s)
	}
	if s := findSymbol(syms, "Count"); s == nil || s.Kind != langfeat.SymbolVar {
		t.Errorf("Count = %+v, want a top-level SymbolVar", s)
	}
	if s := findSymbol(syms, "MaxWidgets"); s == nil || s.Kind != langfeat.SymbolConst {
		t.Errorf("MaxWidgets = %+v, want a top-level SymbolConst", s)
	}
}
