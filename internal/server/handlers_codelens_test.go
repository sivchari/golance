package server

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// codeLensFile returns the absolute path to internal/server/testdata/
// module's pkgDir subpackage's name file.
func codeLensFile(root, pkgDir, name string) string {
	return filepath.Join(root, pkgDir, name)
}

// requestCodeLens calls handleCodeLens for file and returns its result.
func requestCodeLens(t *testing.T, s *Server, file string) []protocol.CodeLens {
	t.Helper()
	result, err := s.handleCodeLens(context.Background(), mustMarshal(t, &protocol.CodeLensParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
	}))
	if err != nil {
		t.Fatalf("handleCodeLens: %v", err)
	}
	lenses, ok := result.([]protocol.CodeLens)
	if !ok {
		t.Fatalf("handleCodeLens = %#v, want []protocol.CodeLens", result)
	}
	return lenses
}

func unmarshalCommandArg[T any](t *testing.T, cmd protocol.Command) T {
	t.Helper()
	var v T
	if len(cmd.Arguments) == 0 {
		t.Fatalf("command %q has no arguments", cmd.Command)
	}
	if err := protocol.Unmarshal(cmd.Arguments[0], &v); err != nil {
		t.Fatalf("unmarshal %q arguments: %v", cmd.Command, err)
	}
	return v
}

func TestHandleCodeLens_Generate(t *testing.T) {
	s, _, root := newTestServer(t)
	lenses := requestCodeLens(t, s, codeLensFile(root, "codelens", "codelens.go"))

	if len(lenses) != 2 {
		t.Fatalf("lenses = %+v, want exactly 2 (recursive + non-recursive go generate)", lenses)
	}
	// Indexed, not "for _, l := range lenses": protocol.CodeLens is a
	// ~120-byte struct (gocritic rangeValCopy), so ranging by value would
	// copy it every iteration.
	for i := range lenses {
		if lenses[i].Command.Command != commandGenerate {
			t.Errorf("Command = %q, want %q", lenses[i].Command.Command, commandGenerate)
		}
	}
	if lenses[0].Range != lenses[1].Range {
		t.Errorf("Range = %+v vs %+v, want equal (same go:generate directive)", lenses[0].Range, lenses[1].Range)
	}
	first := unmarshalCommandArg[generateArgs](t, lenses[0].Command)
	if !first.Recursive {
		t.Error("first lens Recursive = false, want true (gopls emits the recursive command first)")
	}
	second := unmarshalCommandArg[generateArgs](t, lenses[1].Command)
	if second.Recursive {
		t.Error("second lens Recursive = true, want false")
	}
}

func TestHandleCodeLens_RegenerateCgo(t *testing.T) {
	s, _, root := newTestServer(t)
	lenses := requestCodeLens(t, s, codeLensFile(root, "codelenscgo", "codelenscgo.go"))

	if len(lenses) != 1 {
		t.Fatalf("lenses = %+v, want exactly 1 (regenerate cgo)", lenses)
	}
	if lenses[0].Command.Command != commandRegenerateCgo {
		t.Errorf("Command = %q, want %q", lenses[0].Command.Command, commandRegenerateCgo)
	}
}

func TestHandleCodeLens_TestSourceOffByDefault(t *testing.T) {
	s, _, root := newTestServer(t)
	lenses := requestCodeLens(t, s, codeLensFile(root, "codelens", "codelens_test.go"))

	if len(lenses) != 0 {
		t.Fatalf("lenses = %+v, want none (the test code lens source is off by default, matching gopls)", lenses)
	}
}

func TestHandleCodeLens_TestSourceEnabled(t *testing.T) {
	s, _, root := newTestServer(t)
	s.setCodeLensesEnabled(map[codeLensSource]bool{codeLensTest: true})
	lenses := requestCodeLens(t, s, codeLensFile(root, "codelens", "codelens_test.go"))

	var sawTest, sawBenchmark, sawFileBenchmarks int
	// Indexed, not "for _, l := range lenses": protocol.CodeLens is a
	// ~120-byte struct (gocritic rangeValCopy), so ranging by value would
	// copy it every iteration.
	for i := range lenses {
		l := &lenses[i]
		if l.Command.Command != commandRunTests {
			t.Fatalf("unexpected command %q with only the test source enabled", l.Command.Command)
		}
		args := unmarshalCommandArg[runTestsArgs](t, l.Command)
		switch l.Command.Title {
		case "run test":
			sawTest++
			if len(args.Tests) != 1 || args.Tests[0] != "TestAdd" {
				t.Errorf("run test args = %+v, want Tests = [TestAdd]", args)
			}
		case "run benchmark":
			sawBenchmark++
			if len(args.Benchmarks) != 1 || args.Benchmarks[0] != "BenchmarkAdd" {
				t.Errorf("run benchmark args = %+v, want Benchmarks = [BenchmarkAdd]", args)
			}
		case "run file benchmarks":
			sawFileBenchmarks++
			if len(args.Benchmarks) != 1 || args.Benchmarks[0] != "BenchmarkAdd" {
				t.Errorf("run file benchmarks args = %+v, want Benchmarks = [BenchmarkAdd]", args)
			}
		default:
			t.Errorf("unexpected lens title %q", l.Command.Title)
		}
	}
	if sawTest != 1 || sawBenchmark != 1 || sawFileBenchmarks != 1 {
		t.Errorf("saw test=%d benchmark=%d fileBenchmarks=%d, want exactly one of each (FuzzAdd must never lens)", sawTest, sawBenchmark, sawFileBenchmarks)
	}
}

func TestParseCodeLensSettings(t *testing.T) {
	t.Run("nil settings resolve to defaults", func(t *testing.T) {
		got := parseCodeLensSettings(nil)
		if got[codeLensTest] {
			t.Error("test source enabled by default, want disabled")
		}
		if !got[codeLensGenerate] || !got[codeLensRegenerateCgo] {
			t.Error("generate/regenerate_cgo not enabled by default")
		}
	})

	t.Run("explicit override wins, unspecified sources keep their default", func(t *testing.T) {
		raw := mustMarshal(t, map[string]any{"codelenses": map[string]bool{"test": true}})
		got := parseCodeLensSettings(protocol.LSPAny(raw))
		if !got[codeLensTest] {
			t.Error("test source not enabled by explicit override")
		}
		if !got[codeLensGenerate] {
			t.Error("generate source lost its default when only test was overridden")
		}
	})

	t.Run("malformed settings resolve to defaults", func(t *testing.T) {
		got := parseCodeLensSettings(protocol.LSPAny(`{not json`))
		if got[codeLensTest] {
			t.Error("test source enabled from malformed settings, want default (disabled)")
		}
	})
}

func TestHandleExecuteCommand_UnknownCommand(t *testing.T) {
	s, _, _ := newTestServer(t)
	_, err := s.handleExecuteCommand(context.Background(), mustMarshal(t, &protocol.ExecuteCommandParams{Command: "golance.nonexistent"}))
	if err == nil {
		t.Fatal("handleExecuteCommand(unknown) = nil error, want an error")
	}
}

func TestHandleExecuteCommand_Generate(t *testing.T) {
	s, _, root := newTestServer(t)
	dir := filepath.Join(root, "codelens")
	_, err := s.handleExecuteCommand(context.Background(), mustMarshal(t, &protocol.ExecuteCommandParams{
		Command:   commandGenerate,
		Arguments: commandArgs(generateArgs{Dir: uri.File(dir), Recursive: false}),
	}))
	if err != nil {
		t.Fatalf("handleExecuteCommand(generate): %v", err)
	}
}

func TestHandleExecuteCommand_RegenerateCgo(t *testing.T) {
	s, _, root := newTestServer(t)
	file := codeLensFile(root, "codelenscgo", "codelenscgo.go")
	_, err := s.handleExecuteCommand(context.Background(), mustMarshal(t, &protocol.ExecuteCommandParams{
		Command:   commandRegenerateCgo,
		Arguments: commandArgs(uriArg{URI: uri.File(file)}),
	}))
	if err != nil {
		t.Fatalf("handleExecuteCommand(regenerate_cgo): %v", err)
	}
}

func TestHandleExecuteCommand_RunTests(t *testing.T) {
	s, _, root := newTestServer(t)
	file := codeLensFile(root, "codelens", "codelens_test.go")
	_, err := s.handleExecuteCommand(context.Background(), mustMarshal(t, &protocol.ExecuteCommandParams{
		Command:   commandRunTests,
		Arguments: commandArgs(runTestsArgs{URI: uri.File(file), Tests: []string{"TestAdd"}}),
	}))
	if err != nil {
		t.Fatalf("handleExecuteCommand(run_tests): %v", err)
	}
}

func TestDecodeCommandArg_MissingArgument(t *testing.T) {
	var v generateArgs
	if err := decodeCommandArg(nil, &v); err == nil {
		t.Fatal("decodeCommandArg(nil) = nil error, want an error")
	}
}

// TestRunGoTests_ReportsFailure exercises runGoTests directly (not through
// handleExecuteCommand/execRunTests, which never surface a test's own
// pass/fail as an RPC error — see execRunTests' doc) against an ad hoc
// module written on the fly, so this failure case does not need a
// dedicated fixture package whose own `go test` a repo-wide test run would
// otherwise have to skip.
func TestRunGoTests_ReportsFailure(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "go.mod", "module example.com/execrunfail\n\ngo 1.23\n")
	writeTempFile(t, dir, "fail_test.go", "package execrunfail\n\nimport \"testing\"\n\nfunc TestAlwaysFails(t *testing.T) { t.Fatal(\"boom\") }\n")

	s := &Server{logger: newTestLogger(t)}
	var out bytes.Buffer
	failed := s.runGoTests(context.Background(), dir, &out, []string{"TestAlwaysFails"}, nil)
	if failed != 1 {
		t.Fatalf("failed = %d, want 1", failed)
	}
	if !strings.Contains(out.String(), "boom") {
		t.Errorf("output = %q, want it to contain the failing test's own output", out.String())
	}
}

func TestTestNamePattern(t *testing.T) {
	if got := testNamePattern("TestFoo"); got != "^TestFoo$" {
		t.Errorf("testNamePattern(TestFoo) = %q, want ^TestFoo$", got)
	}
	if got := testNamePattern("Test.*"); got != "^$" {
		t.Errorf("testNamePattern(Test.*) = %q, want ^$ (rejects a non-identifier name)", got)
	}
}
