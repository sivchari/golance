package documentlink

import (
	"fmt"

	"example.com/langfeatmod/typedefdep"
)

// Describe uses both imports, one stdlib (external) and one workspace-local.
func Describe(r typedefdep.Remote) string {
	return fmt.Sprintf("%d", r.Value)
}
