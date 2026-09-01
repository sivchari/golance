package withtests

// TestOnlyHelper exists only in this in-package "_test.go" file, to prove
// it joined the checked unit even though packages.Load's non-Tests GoFiles
// list never reports it.
func TestOnlyHelper() int {
	return Value() + 1
}
