// Package rootuser imports a sibling package of the same module — a
// root-to-root import, exercised by TestEngine_Get_RecursiveGetPackage
// ResolvesRootImport to prove Engine.GetPackage can be called recursively
// from inside another package's own recheck without deadlocking.
package rootuser

import "example.com/checkmod/basic"

// UseBasicAdd calls basic.Add.
func UseBasicAdd() int {
	return basic.Add(1, 2)
}
