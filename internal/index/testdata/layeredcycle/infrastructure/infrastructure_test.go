package infrastructure

import (
	"testing"

	"example.com/layeredcycle/usecase"
)

// TestRepository_Load exercises Repository against usecase's own
// constructor — the legal in-package-test-only back-edge to usecase this
// fixture exercises, mirroring approvalagency's real internal/infrastructure
// test files that import internal/usecase.
func TestRepository_Load(t *testing.T) {
	r := &Repository{}
	if r.Load("x").ID != "x" {
		t.Fatal("unexpected id")
	}
	_ = usecase.NewInteractor(nil)
}
