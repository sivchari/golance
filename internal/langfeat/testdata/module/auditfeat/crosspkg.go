package auditfeat

import "example.com/langfeatmod/auditfeat/auditsub"

// UseWidget constructs an auditsub.Widget and returns its ID.
func UseWidget() string {
	w := auditsub.NewWidget("w1")

	return w.ID
}

// UseBox exercises a generic dependency type instantiated with a local
// type argument.
func UseBox() int {
	b := auditsub.Box[int]{Value: 42}

	return b.Value
}
