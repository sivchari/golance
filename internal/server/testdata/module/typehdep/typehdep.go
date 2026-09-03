// Package typehdep is typeh's cross-package fixture: BI/BJ/BS mirror
// typeh's I/J/S exactly, reachable from a supertypes/subtypes query on
// typeh's own types only through the workspace facts index.
package typehdep

// BI declares F.
type BI interface {
	F()
}

// BJ declares F and G.
type BJ interface {
	F()
	G()
}

// BS implements both BI and BJ.
type BS int

func (BS) F() {}
func (BS) G() {}
