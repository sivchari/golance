// Package builtinuse exercises golance's universe/builtin navigation: each
// exported helper below uses exactly one predeclared identifier at a known
// position, matched by its test via a unique surrounding snippet.
package builtinuse

func UseNil(p *int) bool {
	return p == nil
}

func Count(v []int) int {
	return len(v)
}

func Buffer(n int) []byte {
	return make([]byte, n)
}

const (
	FlagA = iota
	FlagB
)

var Answer int

func Describe(err error) string {
	return err.Error()
}
