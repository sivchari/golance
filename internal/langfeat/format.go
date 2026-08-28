package langfeat

import (
	"fmt"
	"go/format"

	"golang.org/x/tools/imports"
)

// Format runs gofmt's algorithm over src.
func Format(src []byte) ([]byte, error) {
	out, err := format.Source(src)
	if err != nil {
		return nil, fmt.Errorf("langfeat: format: %w", err)
	}
	return out, nil
}

// OrganizeImports formats src (as Format does) and additionally adds
// missing and removes unused imports, the same algorithm goimports uses.
// filename's directory influences which imports it can resolve, so it must
// be accurate even though src (not the file on disk) is what gets
// processed.
func OrganizeImports(filename string, src []byte) ([]byte, error) {
	out, err := imports.Process(filename, src, nil)
	if err != nil {
		return nil, fmt.Errorf("langfeat: organize imports for %s: %w", filename, err)
	}
	return out, nil
}
