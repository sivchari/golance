// Package leafa imports shared, so resolving leafa (top's first import)
// resolves shared for the first time too.
package leafa

import "example.com/checkmod/thrash/shared"

// Value returns shared's own value.
func Value() int { return shared.Value() }
