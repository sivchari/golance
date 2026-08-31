// Package typedefdep declares Remote in a separate package from typedef, for
// exercising TypeDefinition's cross-package path.
package typedefdep

// Remote is declared outside the typedef package.
type Remote struct {
	Value int
}
