// Package rpc implements JSON-RPC 2.0 over stdio for the LSP server: framing,
// method dispatch, $/cancelRequest, and the initialize/shutdown/exit
// lifecycle. It is transport plumbing only — handlers own all LSP semantics.
package rpc

import "encoding/json"

const jsonrpcVersion = "2.0"

// message is the wire-level JSON-RPC 2.0 envelope. Method set means a
// request (ID present) or notification (ID absent); Result/Error set means a
// response to one of our own outgoing requests, which this server does not
// currently send.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Code    int32           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (m *message) isRequest() bool      { return m.Method != "" && m.ID != nil }
func (m *message) isNotification() bool { return m.Method != "" && m.ID == nil }
