// Package wrapper mimics a connectrpc-generated API's shape without
// depending on connectrpc: exported struct fields wrapped in generic
// Request[T]/Response[T] types, the pattern a real generated-code monorepo
// package leans on heavily.
package wrapper

// Request wraps an outgoing message of type T.
type Request[T any] struct {
	Msg    T
	Header map[string]string
}

// Response wraps an incoming message of type T.
type Response[T any] struct {
	Msg T
}

// NewRequest builds a Request around msg.
func NewRequest[T any](msg T) *Request[T] {
	return &Request[T]{Msg: msg}
}

// NewResponse builds a Response around msg.
func NewResponse[T any](msg T) *Response[T] {
	return &Response[T]{Msg: msg}
}

// Payload is a stand-in for a generated protobuf message type.
type Payload struct {
	Name string
}

var ExampleReq = NewRequest(Payload{Name: "x"})

func usePayloadName() string {
	return ExampleReq.Msg.Name
}
