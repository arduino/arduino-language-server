package handler

import (
	"context"
	"encoding/json"

	lsp "github.com/sourcegraph/go-lsp"
	"github.com/sourcegraph/jsonrpc2"
)

func readParams(method string, raw *json.RawMessage) (interface{}, error) {
	switch method {
	case "initialize":
		params := new(lsp.InitializeParams)
		err := json.Unmarshal(*raw, params)
		return params, err
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
	case "workspace/didChangeWatchedFiles":
		params := new(lsp.DidChangeWatchedFilesParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "workspace/executeCommand":
		params := new(lsp.ExecuteCommandParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "workspace/applyEdit":
		params := new(ApplyWorkspaceEditParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/publishDiagnostics":
		params := new(lsp.PublishDiagnosticsParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "arduino/selectedBoard":
		params := new(BoardConfig)
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
		result := new([]*commandOrCodeAction)
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
		result := new([]*documentSymbolOrSymbolInformation)
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
	case "workspace/executeCommand":
		result := new(string)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "workspace/applyEdit":
		result := new(ApplyWorkspaceEditResponse)
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

type commandOrCodeAction struct {
	Command    *lsp.Command
	CodeAction *CodeAction
}

func (entry *commandOrCodeAction) UnmarshalJSON(raw []byte) error {
	command := new(lsp.Command)
	err := json.Unmarshal(raw, command)
	if err == nil && len(command.Command) > 0 {
		entry.Command = command
		return nil
	}
	codeAction := new(CodeAction)
	err = json.Unmarshal(raw, codeAction)
	if err != nil {
		return err
	}
	entry.CodeAction = codeAction
	return nil
}

func (entry *commandOrCodeAction) MarshalJSON() ([]byte, error) {
	if entry.Command != nil {
		return json.Marshal(entry.Command)
	}
	if entry.CodeAction != nil {
		return json.Marshal(entry.CodeAction)
	}
	return nil, nil
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

type documentSymbolOrSymbolInformation struct {
	DocumentSymbol    *DocumentSymbol
	SymbolInformation *lsp.SymbolInformation
}

type documentSymbolOrSymbolInformationDiscriminator struct {
	Range    *lsp.Range    `json:"range,omitempty"`
	Location *lsp.Location `json:"location,omitempty"`
}

func (entry *documentSymbolOrSymbolInformation) UnmarshalJSON(raw []byte) error {
	discriminator := new(documentSymbolOrSymbolInformationDiscriminator)
	err := json.Unmarshal(raw, discriminator)
	if err != nil {
		return err
	}
	if discriminator.Range != nil {
		entry.DocumentSymbol = new(DocumentSymbol)
		err = json.Unmarshal(raw, entry.DocumentSymbol)
		if err != nil {
			return err
		}
	}
	if discriminator.Location != nil {
		entry.SymbolInformation = new(lsp.SymbolInformation)
		err = json.Unmarshal(raw, entry.SymbolInformation)
		if err != nil {
			return err
		}
	}
	return nil
}

// ApplyWorkspaceEditParams structure according to LSP
type ApplyWorkspaceEditParams struct {
	Label string            `json:"label,omitempty"`
	Edit  lsp.WorkspaceEdit `json:"edit"`
}

// ApplyWorkspaceEditResponse structure according to LSP
type ApplyWorkspaceEditResponse struct {
	Applied       bool   `json:"applied"`
	FailureReason string `json:"failureReason,omitempty"`
}

// BoardConfig describes the board and port selected by the user.
type BoardConfig struct {
	SelectedBoard Board  `json:"selectedBoard"`
	SelectedPort  string `json:"selectedPort"`
}

// Board structure.
type Board struct {
	Name string `json:"name"`
	Fqbn string `json:"fqbn"`
}
