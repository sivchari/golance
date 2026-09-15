package usecase

import (
	"testing"

	"example.com/layeredcycle/infrastructure"
)

// TestInteractor_Get exercises Interactor against infrastructure's real
// Repository — the legal in-package-test-only back-edge to infrastructure
// this fixture exercises, mirroring approvalagency's real internal/usecase
// test files that import internal/infrastructure.
func TestInteractor_Get(t *testing.T) {
	repo := &infrastructure.Repository{}
	i := NewInteractor(repo)
	if i.Get("x").ID != "x" {
		t.Fatal("unexpected id")
	}
}
