// Package callhdep is callh's fixture for a call into a different
// workspace package (see ../callh/callh.go's Describe), kept separate from
// greet so calling it does not perturb TestHandleReferences' exact
// reference count for greet.Hello.
package callhdep

// Double returns n doubled.
func Double(n int) int {
	return n * 2
}
