package dep

// Box wraps a value of type T, mirroring connectrpc.com/connect's
// Request[T]/Response[T] shape (a Msg *T field) that motivated
// TestProvider_Decl_GenericField and its sibling tests: a field or method
// reached through an instantiated Box[Concrete] is not identity-equal to
// this origin declaration (see Provider.Decl's doc), and Provider must
// still resolve it here.
type Box[T any] struct {
	Msg *T
}

// ValueDescribe is a value-receiver method on the generic Box type.
func (b Box[T]) ValueDescribe() string { return "box" }

// PointerDescribe is a pointer-receiver method on the generic Box type.
func (b *Box[T]) PointerDescribe() string { return "box" }
