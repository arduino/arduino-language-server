package main

import (
	"context"
	"encoding/json"

	lsp "github.com/sourcegraph/go-lsp"
	"github.com/sourcegraph/jsonrpc2"
)

func readParams(method string, raw *json.RawMessage) (interface{}, error) {
	switch method {
	case "textDocument/didOpen":
		params := &lsp.DidOpenTextDocumentParams{}
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/didChange":
		params := &lsp.DidChangeTextDocumentParams{}
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/didSave":
		params := &lsp.DidSaveTextDocumentParams{}
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/didClose":
		params := &lsp.DidCloseTextDocumentParams{}
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/completion":
		params := &lsp.CompletionParams{}
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/codeAction":
		params := &lsp.CodeActionParams{}
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/hover":
		fallthrough
	case "textDocument/definition":
		fallthrough
	case "textDocument/typeDefinition":
		fallthrough
	case "textDocument/implementation":
		fallthrough
	case "textDocument/documentHighlight":
		params := &lsp.TextDocumentPositionParams{}
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/publishDiagnostics":
		params := &lsp.PublishDiagnosticsParams{}
		err := json.Unmarshal(*raw, params)
		return params, err
	}
	return nil, nil
}

func sendRequest(ctx context.Context, conn *jsonrpc2.Conn, method string, params interface{}) (interface{}, error) {
	switch method {
	case "initialize":
		result := &lsp.InitializeResult{}
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "textDocument/completion":
		result := &lsp.CompletionList{}
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "completionItem/resolve":
		result := &lsp.CompletionItem{}
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "textDocument/hover":
		result := &Hover{}
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "textDocument/definition":
		fallthrough
	case "textDocument/typeDefinition":
		fallthrough
	case "textDocument/implementation":
		result := &[]lsp.Location{}
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "textDocument/documentHighlight":
		result := &[]lsp.DocumentHighlight{}
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "window/showMessageRequest":
		result := &lsp.MessageActionItem{}
		err := conn.Call(ctx, method, params, result)
		return result, err
	}
	var result interface{}
	err := conn.Call(ctx, method, params, result)
	return result, err
}

type Hover struct {
	Contents MarkupContent `json:"contents"`
	Range    *lsp.Range    `json:"range,omitempty"`
}

type MarkupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}
