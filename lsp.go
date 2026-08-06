package goplslazy

import "encoding/json"

// JSON-RPC and LSP protocol string constants shared across the proxy.
const (
	jsonrpcVersion = "2.0"

	methodDefinition            = "textDocument/definition"
	methodDidOpen               = "textDocument/didOpen"
	methodDidChange             = "textDocument/didChange"
	methodRename                = "textDocument/rename"
	methodPrepareRename         = "textDocument/prepareRename"
	methodReferences            = "textDocument/references"
	methodImplementation        = "textDocument/implementation"
	methodInlayHint             = "textDocument/inlayHint"
	methodHover                 = "textDocument/hover"
	methodShowMessage           = "window/showMessage"
	methodConfiguration         = "workspace/configuration"
	methodDidChangeWatchedFiles = "workspace/didChangeWatchedFiles"

	// window/showMessage MessageType values (LSP spec).
	messageWarning = 2
	messageInfo    = 3

	// codeRequestFailed is the JSON-RPC error code gopls/LSP use for a request
	// that the server understood but could not fulfil (RequestFailed).
	codeRequestFailed = -32803
)

// requestFailedError builds a JSON-RPC error object carrying the RequestFailed
// code and the given message, for a request the proxy chooses to fail loudly
// (and retryably) rather than answer with silently-partial results.
func requestFailedError(msg string) json.RawMessage {
	b, err := json.Marshal(struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{Code: codeRequestFailed, Message: msg})
	if err != nil {
		return json.RawMessage(`{"code":-32803,"message":"request failed"}`)
	}
	return b
}
