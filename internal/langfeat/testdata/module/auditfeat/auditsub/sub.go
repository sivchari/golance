// Package auditsub is auditfeat's cross-package dependency, for exercising
// hover/completion/definition parity on symbols declared in a different
// package than the query.
package auditsub

// Widget is exported for cross-package hover/completion parity checks.
type Widget struct {
	ID string
}

// NewWidget returns a Widget with the given id.
func NewWidget(id string) *Widget {
	return &Widget{ID: id}
}

// Box wraps a value of type T.
type Box[T any] struct {
	Value T
}
