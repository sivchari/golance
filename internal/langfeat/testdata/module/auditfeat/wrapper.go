package auditfeat

// Request wraps a request message of type T, mirroring connect.Request[T]
// from connectrpc.com/connect.
type Request[T any] struct {
	Msg *T
}

// Any returns the request's underlying message.
func (r *Request[T]) Any() *T { return r.Msg }

// Response wraps a response message of type T, mirroring
// connect.Response[T].
type Response[T any] struct {
	Msg *T
}

// NewResponse constructs a Response wrapping msg.
func NewResponse[T any](msg *T) *Response[T] {
	return &Response[T]{Msg: msg}
}

// Numeric is a constraint satisfied by any integer or floating-point type.
type Numeric interface {
	~int | ~int32 | ~int64 | ~float64
}

// Sum returns the sum of vals.
func Sum[T Numeric](vals ...T) T {
	var total T
	for _, v := range vals {
		total += v
	}

	return total
}
