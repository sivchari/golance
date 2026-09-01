package exttest_test

import "example.com/checkmod/exttest"

// UsesExported resolves fine: Exported is part of exttest's exported API.
func UsesExported() int {
	return exttest.Exported()
}

// UsesUnexported is a deliberate type error: unexported is not part of
// exttest's exported API, so it must not resolve through this external test
// unit's import of exttest, exactly as go/types itself would reject it from
// any other package.
func UsesUnexported() int {
	return exttest.unexported()
}
