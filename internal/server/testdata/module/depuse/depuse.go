// Package depuse imports both the standard library and a different
// workspace package, a fixture for handlers_xref_test.go's
// TestHandleDefinition_Stdlib and TestHandleDefinition_WorkspaceSymbolPreferred:
// "go to definition" on a symbol outside the workspace, and on a
// cross-package workspace symbol, respectively.
package depuse

import (
	"fmt"
	"strings"

	"example.com/servermod/greet"
)

// Describe formats a message using two different standard library packages.
func Describe(name string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("hello, %s", name))
	return b.String()
}

// UseGreet references a workspace-defined type in a different package,
// without calling Hello (see greet.go): TestHandleReferences elsewhere
// counts Hello's exact reference count, which a new caller here would
// perturb.
func UseGreet(g greet.Greeting) string {
	return g.Text
}
