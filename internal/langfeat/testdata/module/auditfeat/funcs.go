package auditfeat

import "context"

// errZero is Divide's zero-divisor error.
type errZero struct{}

func (errZero) Error() string { return "divide by zero" }

var errDivideByZero = errZero{}

// Divide returns a divided by b as (quotient, remainder), or a non-nil
// err if b is zero.
func Divide(a, b int) (quotient, remainder int, err error) {
	if b == 0 {
		err = errDivideByZero
		return
	}
	quotient, remainder = a/b, a%b

	return
}

// Join concatenates parts, separated by sep.
func Join(sep string, parts ...string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}

	return out
}

// Pipeline doubles every value read from in until in closes or ctx is
// done, closing the returned channel afterward.
func Pipeline(ctx context.Context, in <-chan int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for {
			select {
			case v, ok := <-in:
				if !ok {
					return
				}
				out <- v * 2
			case <-ctx.Done():
				return
			}
		}
	}()

	return out
}
