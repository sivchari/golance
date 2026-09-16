// Package outer instantiates modspkg.Set against bobpkg.Mod at package
// scope, mirroring the real-world bob usage pattern that triggers the
// reported false satisfaction failure.
package outer

import (
	"example.com/genericsplit/bobpkg"
	"example.com/genericsplit/modspkg"
	"example.com/genericsplit/shared"
)

var _ bobpkg.Mod[shared.C] = modspkg.MakeDefault()
