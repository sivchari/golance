package broken

import "strings"

// Foo has a Bar field.
type Foo struct {
	Bar string
}

func useFoo(f Foo) {
	f.
}

func useStrings() {
	strings.
}
