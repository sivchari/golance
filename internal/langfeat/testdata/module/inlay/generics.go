package inlay

func Identity[T any](v T) T { return v }

func useGeneric() int {
	return Identity(42)
}
