package rpc

import (
	"errors"
	"fmt"
)

// Error is a JSON-RPC error a handler can return to control the error code
// and structured data sent back to the client. Code should be one of
// protocol.ErrorCodes or protocol.LSPErrorCodes, cast to int32. A handler
// returning a plain error is reported as InternalError.
type Error struct {
	Code    int32
	Message string
	Data    []byte // raw JSON, or nil
}

func (e *Error) Error() string {
	return fmt.Sprintf("rpc: %s (code=%d)", e.Message, e.Code)
}

// NewError constructs an Error with the given JSON-RPC/LSP error code.
func NewError(code int32, message string) *Error {
	return &Error{Code: code, Message: message}
}

// toWireError converts a handler-returned error into an Error, defaulting to
// InternalError (-32603) when err is not already an *Error.
func toWireError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: internalErrorCode, Message: err.Error()}
}
