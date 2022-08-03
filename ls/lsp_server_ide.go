package ls

import (
	"context"
	"io"

	"github.com/fatih/color"
	"go.bug.st/json"
	"go.bug.st/lsp"
	"go.bug.st/lsp/jsonrpc"
)

// IDELSPServer is an IDE lsp server
type IDELSPServer struct {
	conn *lsp.Server
	ls   *INOLanguageServer
}

// NewIDELSPServer creates and return a new server
func NewIDELSPServer(logger jsonrpc.FunctionLogger, in io.Reader, out io.Writer, ls *INOLanguageServer) *IDELSPServer {
	server := &IDELSPServer{
		ls: ls,
	}
	server.conn = lsp.NewServer(in, out, server)
	server.conn.RegisterCustomNotification("ino/didCompleteBuild", server.ArduinoBuildCompleted)
	server.conn.SetLogger(&Logger{
		IncomingPrefix: "IDE --> LS",
		OutgoingPrefix: "IDE <-- LS",
		HiColor:        color.HiGreenString,
		LoColor:        color.GreenString,
		ErrorColor:     color.New(color.BgHiMagenta, color.FgHiWhite, color.BlinkSlow).Sprintf,
	})
	return server
}

// Run runs the server connection
func (server *IDELSPServer) Run() {
	server.conn.Run()
}

// Initialize sends an initilize request
func (server *IDELSPServer) Initialize(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.InitializeParams) (*lsp.InitializeResult, *jsonrpc.ResponseError) {
	return server.ls.InitializeReqFromIDE(ctx, logger, params)
}

// Shutdown sends a shutdown request
func (server *IDELSPServer) Shutdown(ctx context.Context, logger jsonrpc.FunctionLogger) *jsonrpc.ResponseError {
	return server.ls.ShutdownReqFromIDE(ctx, logger)
}

// WorkspaceSymbol is not implemented
func (server *IDELSPServer) WorkspaceSymbol(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.WorkspaceSymbolParams) ([]lsp.SymbolInformation, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// WorkspaceExecuteCommand is not implemented
func (server *IDELSPServer) WorkspaceExecuteCommand(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.ExecuteCommandParams) (json.RawMessage, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// WorkspaceWillCreateFiles is not implemented
func (server *IDELSPServer) WorkspaceWillCreateFiles(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.CreateFilesParams) (*lsp.WorkspaceEdit, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// WorkspaceWillRenameFiles is not implemented
func (server *IDELSPServer) WorkspaceWillRenameFiles(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.RenameFilesParams) (*lsp.WorkspaceEdit, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// WorkspaceWillDeleteFiles is not implemented
func (server *IDELSPServer) WorkspaceWillDeleteFiles(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DeleteFilesParams) (*lsp.WorkspaceEdit, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// TextDocumentWillSaveWaitUntil is not implemented
func (server *IDELSPServer) TextDocumentWillSaveWaitUntil(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.WillSaveTextDocumentParams) ([]lsp.TextEdit, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// TextDocumentCompletion is not implemented
func (server *IDELSPServer) TextDocumentCompletion(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.CompletionParams) (*lsp.CompletionList, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentCompletionReqFromIDE(ctx, logger, params)
}

// CompletionItemResolve is not implemented
func (server *IDELSPServer) CompletionItemResolve(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.CompletionItem) (*lsp.CompletionItem, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// TextDocumentHover sends a request to hover a text document
func (server *IDELSPServer) TextDocumentHover(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.HoverParams) (*lsp.Hover, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentHoverReqFromIDE(ctx, logger, params)
}

// TextDocumentSignatureHelp requests help for text document signature
func (server *IDELSPServer) TextDocumentSignatureHelp(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.SignatureHelpParams) (*lsp.SignatureHelp, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentSignatureHelpReqFromIDE(ctx, logger, params)
}

// TextDocumentDeclaration is not implemented
func (server *IDELSPServer) TextDocumentDeclaration(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DeclarationParams) ([]lsp.Location, []lsp.LocationLink, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// TextDocumentDefinition sends a request to define a text document
func (server *IDELSPServer) TextDocumentDefinition(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DefinitionParams) ([]lsp.Location, []lsp.LocationLink, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentDefinitionReqFromIDE(ctx, logger, params)
}

// TextDocumentTypeDefinition sends a request to define a type for the text document
func (server *IDELSPServer) TextDocumentTypeDefinition(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.TypeDefinitionParams) ([]lsp.Location, []lsp.LocationLink, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentTypeDefinitionReqFromIDE(ctx, logger, params)
}

// TextDocumentImplementation sends a request to implement a text document
func (server *IDELSPServer) TextDocumentImplementation(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.ImplementationParams) ([]lsp.Location, []lsp.LocationLink, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentImplementationReqFromIDE(ctx, logger, params)
}

// TextDocumentReferences is not implemented
func (server *IDELSPServer) TextDocumentReferences(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.ReferenceParams) ([]lsp.Location, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// TextDocumentDocumentHighlight sends a request to highlight a text document
func (server *IDELSPServer) TextDocumentDocumentHighlight(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DocumentHighlightParams) ([]lsp.DocumentHighlight, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentDocumentHighlightReqFromIDE(ctx, logger, params)
}

// TextDocumentDocumentSymbol sends a request for text document symbol
func (server *IDELSPServer) TextDocumentDocumentSymbol(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DocumentSymbolParams) ([]lsp.DocumentSymbol, []lsp.SymbolInformation, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentDocumentSymbolReqFromIDE(ctx, logger, params)
}

// TextDocumentCodeAction sends a request for text document code action
func (server *IDELSPServer) TextDocumentCodeAction(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.CodeActionParams) ([]lsp.CommandOrCodeAction, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentCodeActionReqFromIDE(ctx, logger, params)
}

// CodeActionResolve is not implemented
func (server *IDELSPServer) CodeActionResolve(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.CodeAction) (*lsp.CodeAction, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// TextDocumentCodeLens is not implemented
func (server *IDELSPServer) TextDocumentCodeLens(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.CodeLensParams) ([]lsp.CodeLens, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// CodeLensResolve is not implemented
func (server *IDELSPServer) CodeLensResolve(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.CodeLens) (*lsp.CodeLens, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// TextDocumentDocumentLink is not implemented
func (server *IDELSPServer) TextDocumentDocumentLink(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DocumentLinkParams) ([]lsp.DocumentLink, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// DocumentLinkResolve is not implemented
func (server *IDELSPServer) DocumentLinkResolve(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DocumentLink) (*lsp.DocumentLink, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// TextDocumentDocumentColor is not implemented
func (server *IDELSPServer) TextDocumentDocumentColor(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DocumentColorParams) ([]lsp.ColorInformation, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// TextDocumentColorPresentation is not implemented
func (server *IDELSPServer) TextDocumentColorPresentation(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.ColorPresentationParams) ([]lsp.ColorPresentation, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// TextDocumentFormatting sends a request to format a text document
func (server *IDELSPServer) TextDocumentFormatting(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DocumentFormattingParams) ([]lsp.TextEdit, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentFormattingReqFromIDE(ctx, logger, params)
}

// TextDocumentRangeFormatting sends a request to format the range a text document
func (server *IDELSPServer) TextDocumentRangeFormatting(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DocumentRangeFormattingParams) ([]lsp.TextEdit, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentRangeFormattingReqFromIDE(ctx, logger, params)
}

// TextDocumentOnTypeFormatting is not implemented
func (server *IDELSPServer) TextDocumentOnTypeFormatting(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DocumentOnTypeFormattingParams) ([]lsp.TextEdit, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// TextDocumentRename sends a request to rename a text document
func (server *IDELSPServer) TextDocumentRename(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.RenameParams) (*lsp.WorkspaceEdit, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentRenameReqFromIDE(ctx, logger, params)
}

// TextDocumentFoldingRange is not implemented
func (server *IDELSPServer) TextDocumentFoldingRange(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.FoldingRangeParams) ([]lsp.FoldingRange, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// TextDocumentSelectionRange is not implemented
func (server *IDELSPServer) TextDocumentSelectionRange(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.SelectionRangeParams) ([]lsp.SelectionRange, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// TextDocumentPrepareCallHierarchy is not implemented
func (server *IDELSPServer) TextDocumentPrepareCallHierarchy(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.CallHierarchyPrepareParams) ([]lsp.CallHierarchyItem, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// CallHierarchyIncomingCalls is not implemented
func (server *IDELSPServer) CallHierarchyIncomingCalls(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.CallHierarchyIncomingCallsParams) ([]lsp.CallHierarchyIncomingCall, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// CallHierarchyOutgoingCalls is not implemented
func (server *IDELSPServer) CallHierarchyOutgoingCalls(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.CallHierarchyOutgoingCallsParams) ([]lsp.CallHierarchyOutgoingCall, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// TextDocumentSemanticTokensFull is not implemented
func (server *IDELSPServer) TextDocumentSemanticTokensFull(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.SemanticTokensParams) (*lsp.SemanticTokens, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// TextDocumentSemanticTokensFullDelta is not implemented
func (server *IDELSPServer) TextDocumentSemanticTokensFullDelta(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.SemanticTokensDeltaParams) (*lsp.SemanticTokens, *lsp.SemanticTokensDelta, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// TextDocumentSemanticTokensRange is not implemented
func (server *IDELSPServer) TextDocumentSemanticTokensRange(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.SemanticTokensRangeParams) (*lsp.SemanticTokens, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// WorkspaceSemanticTokensRefresh is not implemented
func (server *IDELSPServer) WorkspaceSemanticTokensRefresh(ctx context.Context, logger jsonrpc.FunctionLogger) *jsonrpc.ResponseError {
	panic("unimplemented")
}

// TextDocumentLinkedEditingRange is not implemented
func (server *IDELSPServer) TextDocumentLinkedEditingRange(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.LinkedEditingRangeParams) (*lsp.LinkedEditingRanges, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// TextDocumentMoniker is not implemented
func (server *IDELSPServer) TextDocumentMoniker(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.MonikerParams) ([]lsp.Moniker, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// Notifications ->

// Progress is not implemented
func (server *IDELSPServer) Progress(logger jsonrpc.FunctionLogger, params *lsp.ProgressParams) {
	panic("unimplemented")
}

// Initialized sends an initialized notification
func (server *IDELSPServer) Initialized(logger jsonrpc.FunctionLogger, params *lsp.InitializedParams) {
	server.ls.InitializedNotifFromIDE(logger, params)
}

// Exit sends an exit notification
func (server *IDELSPServer) Exit(logger jsonrpc.FunctionLogger) {
	server.ls.ExitNotifFromIDE(logger)
}

// SetTrace sends a set trace notification
func (server *IDELSPServer) SetTrace(logger jsonrpc.FunctionLogger, params *lsp.SetTraceParams) {
	server.ls.SetTraceNotifFromIDE(logger, params)
}

// WindowWorkDoneProgressCancel is not implemented
func (server *IDELSPServer) WindowWorkDoneProgressCancel(logger jsonrpc.FunctionLogger, params *lsp.WorkDoneProgressCancelParams) {
	panic("unimplemented")
}

// WorkspaceDidChangeWorkspaceFolders is not implemented
func (server *IDELSPServer) WorkspaceDidChangeWorkspaceFolders(logger jsonrpc.FunctionLogger, params *lsp.DidChangeWorkspaceFoldersParams) {
	panic("unimplemented")
}

// WorkspaceDidChangeConfiguration purpose is explained below
func (server *IDELSPServer) WorkspaceDidChangeConfiguration(logger jsonrpc.FunctionLogger, params *lsp.DidChangeConfigurationParams) {
	// At least one LSP client, Eglot, sends this by default when
	// first connecting, even if the otions are empty.
	// https://github.com/joaotavora/eglot/blob/e835996e16610d0ded6d862214b3b452b8803ea8/eglot.el#L1080
	//
	// Since ALS doesn’t have any workspace configuration yet,
	// ignore it.
	return

}

// WorkspaceDidChangeWatchedFiles is not implemented
func (server *IDELSPServer) WorkspaceDidChangeWatchedFiles(logger jsonrpc.FunctionLogger, params *lsp.DidChangeWatchedFilesParams) {
	panic("unimplemented")
}

// WorkspaceDidCreateFiles is not implemented
func (server *IDELSPServer) WorkspaceDidCreateFiles(logger jsonrpc.FunctionLogger, params *lsp.CreateFilesParams) {
	panic("unimplemented")
}

// WorkspaceDidRenameFiles is not implemented
func (server *IDELSPServer) WorkspaceDidRenameFiles(logger jsonrpc.FunctionLogger, params *lsp.RenameFilesParams) {
	panic("unimplemented")
}

// WorkspaceDidDeleteFiles is not implemented
func (server *IDELSPServer) WorkspaceDidDeleteFiles(logger jsonrpc.FunctionLogger, params *lsp.DeleteFilesParams) {
	panic("unimplemented")
}

// TextDocumentDidOpen sends a notification the a text document is open
func (server *IDELSPServer) TextDocumentDidOpen(logger jsonrpc.FunctionLogger, params *lsp.DidOpenTextDocumentParams) {
	server.ls.TextDocumentDidOpenNotifFromIDE(logger, params)
}

// TextDocumentDidChange sends a notification the a text document has changed
func (server *IDELSPServer) TextDocumentDidChange(logger jsonrpc.FunctionLogger, params *lsp.DidChangeTextDocumentParams) {
	server.ls.TextDocumentDidChangeNotifFromIDE(logger, params)
}

// TextDocumentWillSave is not implemented
func (server *IDELSPServer) TextDocumentWillSave(logger jsonrpc.FunctionLogger, params *lsp.WillSaveTextDocumentParams) {
	panic("unimplemented")
}

// TextDocumentDidSave sends a notification the a text document has been saved
func (server *IDELSPServer) TextDocumentDidSave(logger jsonrpc.FunctionLogger, params *lsp.DidSaveTextDocumentParams) {
	server.ls.TextDocumentDidSaveNotifFromIDE(logger, params)
}

// TextDocumentDidClose sends a notification the a text document has been closed
func (server *IDELSPServer) TextDocumentDidClose(logger jsonrpc.FunctionLogger, params *lsp.DidCloseTextDocumentParams) {
	server.ls.TextDocumentDidCloseNotifFromIDE(logger, params)
}

// DidCompleteBuildParams is a custom notification from the Arduino IDE, sent
type DidCompleteBuildParams struct {
	BuildOutputURI *lsp.DocumentURI `json:"buildOutputUri"`
}

func (server *IDELSPServer) ArduinoBuildCompleted(logger jsonrpc.FunctionLogger, raw json.RawMessage) {
	if !server.ls.config.SkipLibrariesDiscoveryOnRebuild {
		return
	}

	var params DidCompleteBuildParams
	if err := json.Unmarshal(raw, &params); err != nil {
		logger.Logf("ERROR decoding DidCompleteBuildParams: %s", err)
	} else {
		server.ls.FullBuildCompletedFromIDE(logger, &params)
	}
}
