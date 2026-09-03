package langfeat_test

import (
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

func TestFindGenerateLens(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "codelens", "codelens.go")

	lens, ok, err := langfeat.FindGenerateLens(cp, path)
	if err != nil {
		t.Fatalf("FindGenerateLens: %v", err)
	}
	if !ok {
		t.Fatal("FindGenerateLens = false, want a lens for the file's go:generate directive")
	}
	if lens.Range.StartOffset == lens.Range.EndOffset {
		t.Errorf("Range is zero-width (%+v), want a span covering \"//go:generate\"", lens.Range)
	}
	if lens.Range.EndOffset-lens.Range.StartOffset != len("//go:generate") {
		t.Errorf("Range span = %d bytes, want %d (len(\"//go:generate\"))", lens.Range.EndOffset-lens.Range.StartOffset, len("//go:generate"))
	}
}

func TestFindGenerateLens_NoDirective(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "callhierarchy", "callhierarchy.go")

	_, ok, err := langfeat.FindGenerateLens(cp, path)
	if err != nil {
		t.Fatalf("FindGenerateLens: %v", err)
	}
	if ok {
		t.Error("FindGenerateLens = true for a file with no go:generate directive")
	}
}

func TestFindRegenerateCgoLens(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "codelenscgo", "codelenscgo.go")

	lens, ok, err := langfeat.FindRegenerateCgoLens(cp, path)
	if err != nil {
		t.Fatalf("FindRegenerateCgoLens: %v", err)
	}
	if !ok {
		t.Fatal("FindRegenerateCgoLens = false, want a lens for the file's import \"C\"")
	}
	if lens.Range.StartOffset >= lens.Range.EndOffset {
		t.Errorf("Range = %+v, want a non-empty span covering the import spec", lens.Range)
	}
}

func TestFindRegenerateCgoLens_NoImportC(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "codelens", "codelens.go")

	_, ok, err := langfeat.FindRegenerateCgoLens(cp, path)
	if err != nil {
		t.Fatalf("FindRegenerateCgoLens: %v", err)
	}
	if ok {
		t.Error("FindRegenerateCgoLens = true for a file with no import \"C\"")
	}
}

func findTestFuncLens(lenses []langfeat.TestFuncLens, name string) (langfeat.TestFuncLens, bool) {
	for _, l := range lenses {
		if l.Name == name {
			return l, true
		}
	}
	return langfeat.TestFuncLens{}, false
}

func TestTestAndBenchmarkLenses(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "codelens", "codelens_test.go")

	tests, benchmarks, err := langfeat.TestAndBenchmarkLenses(cp, path)
	if err != nil {
		t.Fatalf("TestAndBenchmarkLenses: %v", err)
	}

	t.Run("recognizes a genuine test func", func(t *testing.T) {
		lens, ok := findTestFuncLens(tests, "TestAdd")
		if !ok {
			t.Fatalf("tests = %+v, want TestAdd", tests)
		}
		if lens.Range.StartOffset != lens.Range.EndOffset {
			t.Errorf("TestAdd Range = %+v, want zero-width", lens.Range)
		}
	})

	t.Run("recognizes a genuine benchmark func", func(t *testing.T) {
		if _, ok := findTestFuncLens(benchmarks, "BenchmarkAdd"); !ok {
			t.Fatalf("benchmarks = %+v, want BenchmarkAdd", benchmarks)
		}
	})

	t.Run("rejects a name-only match with the wrong parameter type", func(t *testing.T) {
		if _, ok := findTestFuncLens(tests, "TestHelper"); ok {
			t.Error("tests contains TestHelper, want excluded (parameter is not *testing.T)")
		}
	})

	t.Run("rejects a name-only match with too many parameters", func(t *testing.T) {
		if _, ok := findTestFuncLens(tests, "TestTooManyParams"); ok {
			t.Error("tests contains TestTooManyParams, want excluded (more than one parameter)")
		}
	})

	t.Run("rejects a name that fails the Test regexp", func(t *testing.T) {
		if _, ok := findTestFuncLens(tests, "Testable"); ok {
			t.Error("tests contains Testable, want excluded (fails ^Test([^a-z]|$))")
		}
	})

	t.Run("does not lens Fuzz functions", func(t *testing.T) {
		if _, ok := findTestFuncLens(tests, "FuzzAdd"); ok {
			t.Error("tests contains FuzzAdd, want excluded (gopls has no fuzz lens source)")
		}
		if _, ok := findTestFuncLens(benchmarks, "FuzzAdd"); ok {
			t.Error("benchmarks contains FuzzAdd, want excluded")
		}
	})

	t.Run("does not lens Example functions", func(t *testing.T) {
		if _, ok := findTestFuncLens(tests, "ExampleAdd"); ok {
			t.Error("tests contains ExampleAdd, want excluded (gopls has no example lens source)")
		}
	})

	t.Run("does not lens a subtest registered via t.Run", func(t *testing.T) {
		if _, ok := findTestFuncLens(tests, "subtest"); ok {
			t.Error("tests contains a subtest entry, want none (subtests are never top-level FuncDecls)")
		}
	})
}

func TestTestAndBenchmarkLenses_NonTestFile(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "codelens", "codelens.go")

	tests, benchmarks, err := langfeat.TestAndBenchmarkLenses(cp, path)
	if err != nil {
		t.Fatalf("TestAndBenchmarkLenses: %v", err)
	}
	if tests != nil || benchmarks != nil {
		t.Errorf("TestAndBenchmarkLenses(non-test file) = (%+v, %+v), want (nil, nil)", tests, benchmarks)
	}
}

func TestFileBenchmarksRange(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "codelens", "codelens_test.go")

	rng, err := langfeat.FileBenchmarksRange(cp, path)
	if err != nil {
		t.Fatalf("FileBenchmarksRange: %v", err)
	}
	if rng.StartOffset != rng.EndOffset {
		t.Errorf("FileBenchmarksRange = %+v, want zero-width", rng)
	}
}
