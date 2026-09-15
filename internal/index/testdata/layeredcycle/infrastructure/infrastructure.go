// Package infrastructure implements domain-backed persistence, with no
// production dependency on usecase — mirroring approvalagency's own
// internal/infrastructure package.
package infrastructure

import "example.com/layeredcycle/domain"

// Repository loads a domain.Entity.
type Repository struct{}

// Load returns an Entity for id.
func (r *Repository) Load(id string) domain.Entity {
	return domain.Entity{ID: id}
}
