package lsp

import (
	"context"
	"encoding/json"

	"github.com/sourcegraph/jsonrpc2"
)

func ReadParams(method string, raw *json.RawMessage) (interface{}, error) {
	switch method {
	case "initialize":
		params := new(InitializeParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/didOpen":
		params := new(DidOpenTextDocumentParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/didChange":
		params := new(DidChangeTextDocumentParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/didSave":
		params := new(DidSaveTextDocumentParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/didClose":
		params := new(DidCloseTextDocumentParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/completion":
		params := new(CompletionParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/codeAction":
		params := new(CodeActionParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/signatureHelp":
		fallthrough
	case "textDocument/hover":
		params := new(HoverParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/definition":
		fallthrough
	case "textDocument/typeDefinition":
		fallthrough
	case "textDocument/implementation":
		fallthrough
	case "textDocument/documentHighlight":
		params := new(TextDocumentPositionParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/references":
		params := new(ReferenceParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/formatting":
		params := new(DocumentFormattingParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/rangeFormatting":
		params := new(DocumentRangeFormattingParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/onTypeFormatting":
		params := new(DocumentOnTypeFormattingParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/documentSymbol":
		params := new(DocumentSymbolParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/rename":
		params := new(RenameParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "workspace/symbol":
		params := new(WorkspaceSymbolParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "workspace/didChangeWatchedFiles":
		params := new(DidChangeWatchedFilesParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "workspace/executeCommand":
		params := new(ExecuteCommandParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "workspace/applyEdit":
		params := new(ApplyWorkspaceEditParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "textDocument/publishDiagnostics":
		params := new(PublishDiagnosticsParams)
		err := json.Unmarshal(*raw, params)
		return params, err
	case "arduino/selectedBoard":
		params := new(BoardConfig)
		err := json.Unmarshal(*raw, params)
		return params, err
	}
	return nil, nil
}

func SendRequest(ctx context.Context, conn *jsonrpc2.Conn, method string, params interface{}) (interface{}, error) {
	switch method {
	case "initialize":
		result := new(InitializeResult)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "textDocument/completion":
		result := new(CompletionList)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "textDocument/codeAction":
		result := new([]*CommandOrCodeAction)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "completionItem/resolve":
		result := new(CompletionItem)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "textDocument/signatureHelp":
		result := new(SignatureHelp)
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
		result := new([]Location)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "textDocument/documentHighlight":
		result := new([]DocumentHighlight)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "textDocument/formatting":
		fallthrough
	case "textDocument/rangeFormatting":
		fallthrough
	case "textDocument/onTypeFormatting":
		result := new([]TextEdit)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "textDocument/documentSymbol":
		result := new([]*DocumentSymbolOrSymbolInformation)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "textDocument/rename":
		result := new(WorkspaceEdit)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "workspace/symbol":
		result := new([]SymbolInformation)
		err := conn.Call(ctx, method, params, result)
		return result, err
	case "window/showMessageRequest":
		result := new(MessageActionItem)
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
	Title       string         `json:"title"`
	Kind        string         `json:"kind,omitempty"`
	Diagnostics []Diagnostic   `json:"diagnostics,omitempty"`
	Edit        *WorkspaceEdit `json:"edit,omitempty"`
	Command     *Command       `json:"command,omitempty"`
}

type CommandOrCodeAction struct {
	Command    *Command
	CodeAction *CodeAction
}

func (entry *CommandOrCodeAction) UnmarshalJSON(raw []byte) error {
	command := new(Command)
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

func (entry *CommandOrCodeAction) MarshalJSON() ([]byte, error) {
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
	Range    *Range        `json:"range,omitempty"`
}

// HoverParams structure according to LSP
type HoverParams struct {
	TextDocumentPositionParams
	// WorkDoneProgressParams
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
	Kind           SymbolKind       `json:"kind"`
	Deprecated     bool             `json:"deprecated,omitempty"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}

type DocumentSymbolOrSymbolInformation struct {
	DocumentSymbol    *DocumentSymbol
	SymbolInformation *SymbolInformation
}

type documentSymbolOrSymbolInformationDiscriminator struct {
	Range    *Range    `json:"range,omitempty"`
	Location *Location `json:"location,omitempty"`
}

func (entry *DocumentSymbolOrSymbolInformation) UnmarshalJSON(raw []byte) error {
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
		entry.SymbolInformation = new(SymbolInformation)
		err = json.Unmarshal(raw, entry.SymbolInformation)
		if err != nil {
			return err
		}
	}
	return nil
}

// ApplyWorkspaceEditParams structure according to LSP
type ApplyWorkspaceEditParams struct {
	Label string        `json:"label,omitempty"`
	Edit  WorkspaceEdit `json:"edit"`
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
