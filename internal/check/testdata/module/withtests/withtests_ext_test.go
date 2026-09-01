package withtests_test

// ExternalOnly exists only in this external "_test" package variant, and
// must not join the base package's checked unit (external test packages
// are Phase 2, out of scope for this change).
func ExternalOnly() int {
	return 2
}
