// Package usecase implements business logic over domain, with no
// production dependency on infrastructure — mirroring approvalagency's own
// internal/usecase package.
package usecase

import "example.com/layeredcycle/domain"

// Repository abstracts infrastructure's persistence for usecase.
type Repository interface {
	Load(id string) domain.Entity
}

// Interactor runs usecase logic against a Repository.
type Interactor struct {
	repo Repository
}

// NewInteractor returns an Interactor over repo.
func NewInteractor(repo Repository) *Interactor {
	return &Interactor{repo: repo}
}

// Get returns the Entity for id.
func (i *Interactor) Get(id string) domain.Entity {
	return i.repo.Load(id)
}
