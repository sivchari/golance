// Package user2 references impl.Person as a return type only.
package user2

import "example.com/xrefmod/impl"

// Use2 references impl.Person as a return type and impl.NewPerson as a call.
func Use2() impl.Person {
	return impl.NewPerson("c")
}
