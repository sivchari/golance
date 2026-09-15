// Package service wires usecase and infrastructure together in production,
// standing in for one of approvalagency's own downstream dependents of the
// usecase/infrastructure test-only cycle below.
package service

import (
	"example.com/layeredcycle/infrastructure"
	"example.com/layeredcycle/usecase"
)

// New returns an Interactor backed by a real Repository.
func New() *usecase.Interactor {
	return usecase.NewInteractor(&infrastructure.Repository{})
}
