// Package payload provides the type argument wrapper.Box is instantiated
// with in the consumer package — a ROOT package, unlike
// testdata/genericclosure's payload.
package payload

// Data is a payload type.
type Data struct {
	Field int
}
