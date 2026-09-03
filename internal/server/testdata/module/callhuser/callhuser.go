// Package callhuser calls into ../callh from a different package, for
// callHierarchy/incomingCalls' "cross-package incoming (only findable via
// the workspace facts index)" case.
package callhuser

import "example.com/servermod/callh"

// UseAdd calls callh.Add from a different package.
func UseAdd() int {
	return callh.Add(10, 20)
}
