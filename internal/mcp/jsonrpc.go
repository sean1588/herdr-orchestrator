// Package mcp serves orchestrator state and per-task control over a loopback MCP
// endpoint: hand-rolled JSON-RPC 2.0 (the request/response subset of MCP's
// Streamable HTTP transport) on stdlib net/http, with zero non-stdlib
// dependencies. Reads hit the store's single-writer-safe reads; control routes
// through the injected Controller (the scheduler command seam). The server is
// mounted in-process by `orchestratord daemon` and binds loopback only.
package mcp

import "encoding/json"

// JSON-RPC 2.0 standard error codes.
const (
	codeParse       = -32700
	codeInvalidReq  = -32600
	codeMethodNotFn = -32601
	codeInvalidPar  = -32602
	codeInternal    = -32603
)

// request is one JSON-RPC message. An absent id (len 0) marks a notification,
// which receives no response.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"` // always emitted (null when the request id is unknown), per JSON-RPC 2.0
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func okResp(id json.RawMessage, result any) response {
	return response{JSONRPC: "2.0", ID: id, Result: result}
}

func errResp(id json.RawMessage, code int, msg string) response {
	return response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// mustMarshal marshals a response, falling back to an internal-error envelope if
// marshalling somehow fails (it should not for these types).
func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(errResp(nil, codeInternal, "marshal error"))
	}
	return b
}
