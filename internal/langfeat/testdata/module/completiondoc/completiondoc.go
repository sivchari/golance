package completiondoc

import "example.com/langfeatmod/typedefdep"

// Widget has a documented field.
type Widget struct {
	// Size is the widget's size.
	Size int
}

func selectorSite(w Widget) int {
	return w.Size
}

// Helper is a documented package-level function, for the lexical-scope
// completion doc test.
func Helper() int {
	return 0
}

func lexicalSite() int {
	return Helper()
}

func packageMemberSite() typedefdep.Remote {
	var r typedefdep.Remote
	return r
}
