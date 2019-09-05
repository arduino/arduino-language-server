package handler

import (
	"context"
	"encoding/json"

	lsp "github.com/sourcegraph/go-lsp"
	"github.com/sourcegraph/jsonrpc2"
)

func readParams(method string, raw *json.RawMessage) (interface{}, error) {
	switch method {
	case "textDocument/didOpen":
		params := new(lsp.DidOpenTextDocumentParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/didChange":
		params := new(lsp.DidChangeTextDocumentParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/didSave":
		params := new(lsp.DidSaveTextDocumentParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/didClose":
		params := new(lsp.DidCloseTextDocumentParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/completion":
		params := new(lsp.CompletionParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/codeAction":
		params := new(lsp.CodeActionParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/signatureHelp":
		fallthrough
	case "textDocument/hover":
		fallthrough
	case "textDocument/definition":
		fallthrough
	case "textDocument/typeDefinition":
		fallthrough
	case "textDocument/implementation":
		fallthrough
	case "textDocument/documentHighlight":
		params := new(lsp.TextDocumentPositionParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/references":
		params := new(lsp.ReferenceParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/formatting":
		params := new(lsp.DocumentFormattingParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/rangeFormatting":
		params := new(lsp.DocumentRangeFormattingParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/onTypeFormatting":
		params := new(lsp.DocumentOnTypeFormattingParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/documentSymbol":
		params := new(lsp.DocumentSymbolParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/rename":
		params := new(lsp.RenameParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "workspace/symbol":
		params := new(lsp.WorkspaceSymbolParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/publishDiagnostics":
		params := new(lsp.PublishDiagnosticsParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	}
	return nil, nil
}

func sendRequest(ctx context.Context, conn *jsonrpc2.Conn, method string, params interface{}) (interface{}, error) {
	switch method {
	case "initialize":
		result := new(lsp.InitializeResult)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "textDocument/completion":
		result := new(lsp.CompletionList)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "textDocument/codeAction":
		result := new([]CodeAction)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "completionItem/resolve":
		result := new(lsp.CompletionItem)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "textDocument/signatureHelp":
		result := new(lsp.SignatureHelp)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "textDocument/hover":
		result := new(Hover)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "textDocument/definition":
		fallthrough
	case "textDocument/typeDefinition":
		fallthrough
	case "textDocument/implementation":
		fallthrough
	case "textDocument/references":
		result := new([]lsp.Location)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "textDocument/documentHighlight":
		result := new([]lsp.DocumentHighlight)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "textDocument/formatting":
		fallthrough
	case "textDocument/rangeFormatting":
		fallthrough
	case "textDocument/onTypeFormatting":
		result := new([]lsp.TextEdit)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "textDocument/documentSymbol":
		result := new([]DocumentSymbol)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "textDocument/rename":
		result := new(lsp.WorkspaceEdit)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "workspace/symbol":
		result := new([]lsp.SymbolInformation)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "window/showMessageRequest":
		result := new(lsp.MessageActionItem)
		err := conn.Call(ctx, method, params, result)
		return result, err
	}
	var result interface{}
	err := conn.Call(ctx, method, params, result)
	return result, err
}

// CodeAction structure according to LSP
type CodeAction struct {
	Title       string             `json:"title"`
	Kind        string             `json:"kind,omitempty"`
	Diagnostics []lsp.Diagnostic   `json:"diagnostics,omitempty"`
	Edit        *lsp.WorkspaceEdit `json:"edit,omitempty"`
	Command     *lsp.Command       `json:"command,omitempty"`
}

// Hover structure according to LSP
type Hover struct {
	Contents MarkupContent `json:"contents"`
	Range    *lsp.Range    `json:"range,omitempty"`
}

// MarkupContent structure according to LSP
type MarkupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// DocumentSymbol structure according to LSP
type DocumentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail,omitempty"`
	Kind           lsp.SymbolKind   `json:"kind"`
	Deprecated     bool             `json:"deprecated,omitempty"`
	Range          lsp.Range        `json:"range"`
	SelectionRange lsp.Range        `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}
