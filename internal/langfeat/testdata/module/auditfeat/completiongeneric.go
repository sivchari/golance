package auditfeat

import "example.com/langfeatmod/auditfeat/auditsub"

// useBoxCompletionOnDisk exercises member completion on a cross-package
// generic instantiation (auditsub.Box[int]) when the file is already on
// disk -- see TestCompletion_CrossPackageGenericInstantiation. Mirrors
// TestCompletion_CrossPackageGenericInstantiationOverlay's otherwise
// identical overlay-only case.
func useBoxCompletionOnDisk() {
	b := auditsub.Box[int]{Value: 42}
	_ = b.
}
