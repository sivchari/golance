// Package depfixture is a fixture module with a real external module-cache
// dependency (golang.org/x/sync/singleflight, already required by golance
// itself — see the repo's own go.mod), used to test that graph.Load
// resolves dependency export data without requesting NeedExportFile on the
// main "./..." load (see loadDepExportFiles).
package depfixture

import "golang.org/x/sync/singleflight"

// Group re-exports singleflight.Group so this package has a real,
// non-trivial reference to its one external dependency.
type Group = singleflight.Group
