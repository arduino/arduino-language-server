package ls

import (
	"context"
	"io"

	"github.com/fatih/color"
	"go.bug.st/json"
	"go.bug.st/lsp"
	"go.bug.st/lsp/jsonrpc"
)

type IDELSPServer struct {
	conn *lsp.Server
	ls   *INOLanguageServer
}

func NewIDELSPServer(logger jsonrpc.FunctionLogger, in io.Reader, out io.Writer, ls *INOLanguageServer) *IDELSPServer {
	server := &IDELSPServer{
		ls: ls,
	}
	server.conn = lsp.NewServer(in, out, server)
	server.conn.SetLogger(&LSPLogger{
		IncomingPrefix: "IDE --> LS",
		OutgoingPrefix: "IDE <-- LS",
		HiColor:        color.HiGreenString,
		LoColor:        color.GreenString,
		ErrorColor:     color.New(color.BgHiMagenta, color.FgHiWhite, color.BlinkSlow).Sprintf,
	})
	return server
}

func (server *IDELSPServer) Run() {
	server.conn.Run()
}

func (server *IDELSPServer) Initialize(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.InitializeParams) (*lsp.InitializeResult, *jsonrpc.ResponseError) {
	return server.ls.InitializeReqFromIDE(ctx, logger, params)
}

func (server *IDELSPServer) Shutdown(ctx context.Context, logger jsonrpc.FunctionLogger) *jsonrpc.ResponseError {
	return server.ls.ShutdownReqFromIDE(ctx, logger)
}

func (server *IDELSPServer) WorkspaceSymbol(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.WorkspaceSymbolParams) ([]lsp.SymbolInformation, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) WorkspaceExecuteCommand(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.ExecuteCommandParams) (json.RawMessage, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) WorkspaceWillCreateFiles(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.CreateFilesParams) (*lsp.WorkspaceEdit, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) WorkspaceWillRenameFiles(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.RenameFilesParams) (*lsp.WorkspaceEdit, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) WorkspaceWillDeleteFiles(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DeleteFilesParams) (*lsp.WorkspaceEdit, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentWillSaveWaitUntil(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.WillSaveTextDocumentParams) ([]lsp.TextEdit, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentCompletion(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.CompletionParams) (*lsp.CompletionList, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentCompletionReqFromIDE(ctx, logger, params)
}

func (server *IDELSPServer) CompletionItemResolve(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.CompletionItem) (*lsp.CompletionItem, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentHover(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.HoverParams) (*lsp.Hover, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentHoverReqFromIDE(ctx, logger, params)
}

func (server *IDELSPServer) TextDocumentSignatureHelp(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.SignatureHelpParams) (*lsp.SignatureHelp, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentSignatureHelpReqFromIDE(ctx, logger, params)
}

func (server *IDELSPServer) TextDocumentDeclaration(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DeclarationParams) ([]lsp.Location, []lsp.LocationLink, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentDefinition(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DefinitionParams) ([]lsp.Location, []lsp.LocationLink, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentDefinitionReqFromIDE(ctx, logger, params)
}

func (server *IDELSPServer) TextDocumentTypeDefinition(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.TypeDefinitionParams) ([]lsp.Location, []lsp.LocationLink, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentTypeDefinitionReqFromIDE(ctx, logger, params)
}

func (server *IDELSPServer) TextDocumentImplementation(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.ImplementationParams) ([]lsp.Location, []lsp.LocationLink, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentImplementationReqFromIDE(ctx, logger, params)
}

func (server *IDELSPServer) TextDocumentReferences(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.ReferenceParams) ([]lsp.Location, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentDocumentHighlight(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DocumentHighlightParams) ([]lsp.DocumentHighlight, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentDocumentHighlightReqFromIDE(ctx, logger, params)
}

func (server *IDELSPServer) TextDocumentDocumentSymbol(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DocumentSymbolParams) ([]lsp.DocumentSymbol, []lsp.SymbolInformation, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentDocumentSymbolReqFromIDE(ctx, logger, params)
}

func (server *IDELSPServer) TextDocumentCodeAction(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.CodeActionParams) ([]lsp.CommandOrCodeAction, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentCodeActionReqFromIDE(ctx, logger, params)
}

func (server *IDELSPServer) CodeActionResolve(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.CodeAction) (*lsp.CodeAction, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentCodeLens(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.CodeLensParams) ([]lsp.CodeLens, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) CodeLensResolve(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.CodeLens) (*lsp.CodeLens, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentDocumentLink(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DocumentLinkParams) ([]lsp.DocumentLink, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) DocumentLinkResolve(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DocumentLink) (*lsp.DocumentLink, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentDocumentColor(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DocumentColorParams) ([]lsp.ColorInformation, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentColorPresentation(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.ColorPresentationParams) ([]lsp.ColorPresentation, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentFormatting(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DocumentFormattingParams) ([]lsp.TextEdit, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentFormattingReqFromIDE(ctx, logger, params)
}

func (server *IDELSPServer) TextDocumentRangeFormatting(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DocumentRangeFormattingParams) ([]lsp.TextEdit, *jsonrpc.ResponseError) {
	return server.ls.TextDocumentRangeFormattingReqFromIDE(ctx, logger, params)
}

func (server *IDELSPServer) TextDocumentOnTypeFormatting(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.DocumentOnTypeFormattingParams) ([]lsp.TextEdit, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentRename(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.RenameParams) (*lsp.WorkspaceEdit, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentFoldingRange(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.FoldingRangeParams) ([]lsp.FoldingRange, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentSelectionRange(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.SelectionRangeParams) ([]lsp.SelectionRange, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentPrepareCallHierarchy(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.CallHierarchyPrepareParams) ([]lsp.CallHierarchyItem, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) CallHierarchyIncomingCalls(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.CallHierarchyIncomingCallsParams) ([]lsp.CallHierarchyIncomingCall, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) CallHierarchyOutgoingCalls(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.CallHierarchyOutgoingCallsParams) ([]lsp.CallHierarchyOutgoingCall, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentSemanticTokensFull(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.SemanticTokensParams) (*lsp.SemanticTokens, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentSemanticTokensFullDelta(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.SemanticTokensDeltaParams) (*lsp.SemanticTokens, *lsp.SemanticTokensDelta, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentSemanticTokensRange(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.SemanticTokensRangeParams) (*lsp.SemanticTokens, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) WorkspaceSemanticTokensRefresh(ctx context.Context, logger jsonrpc.FunctionLogger) *jsonrpc.ResponseError {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentLinkedEditingRange(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.LinkedEditingRangeParams) (*lsp.LinkedEditingRanges, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentMoniker(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.MonikerParams) ([]lsp.Moniker, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// Notifications ->

func (server *IDELSPServer) Progress(logger jsonrpc.FunctionLogger, params *lsp.ProgressParams) {
	panic("unimplemented")
}

func (server *IDELSPServer) Initialized(logger jsonrpc.FunctionLogger, params *lsp.InitializedParams) {
	server.ls.InitializedNotifFromIDE(logger, params)
}

func (server *IDELSPServer) Exit(logger jsonrpc.FunctionLogger) {
	server.ls.ExitNotifFromIDE(logger)
}

func (server *IDELSPServer) SetTrace(logger jsonrpc.FunctionLogger, params *lsp.SetTraceParams) {
	server.ls.SetTraceNotifFromIDE(logger, params)
}

func (server *IDELSPServer) WindowWorkDoneProgressCancel(logger jsonrpc.FunctionLogger, params *lsp.WorkDoneProgressCancelParams) {
	panic("unimplemented")
}

func (server *IDELSPServer) WorkspaceDidChangeWorkspaceFolders(logger jsonrpc.FunctionLogger, params *lsp.DidChangeWorkspaceFoldersParams) {
	panic("unimplemented")
}

func (server *IDELSPServer) WorkspaceDidChangeConfiguration(logger jsonrpc.FunctionLogger, params *lsp.DidChangeConfigurationParams) {
	panic("unimplemented")
}

func (server *IDELSPServer) WorkspaceDidChangeWatchedFiles(logger jsonrpc.FunctionLogger, params *lsp.DidChangeWatchedFilesParams) {
	panic("unimplemented")
}

func (server *IDELSPServer) WorkspaceDidCreateFiles(logger jsonrpc.FunctionLogger, params *lsp.CreateFilesParams) {
	panic("unimplemented")
}

func (server *IDELSPServer) WorkspaceDidRenameFiles(logger jsonrpc.FunctionLogger, params *lsp.RenameFilesParams) {
	panic("unimplemented")
}

func (server *IDELSPServer) WorkspaceDidDeleteFiles(logger jsonrpc.FunctionLogger, params *lsp.DeleteFilesParams) {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentDidOpen(logger jsonrpc.FunctionLogger, params *lsp.DidOpenTextDocumentParams) {
	server.ls.TextDocumentDidOpenNotifFromIDE(logger, params)
}

func (server *IDELSPServer) TextDocumentDidChange(logger jsonrpc.FunctionLogger, params *lsp.DidChangeTextDocumentParams) {
	server.ls.TextDocumentDidChangeNotifFromIDE(logger, params)
}

func (server *IDELSPServer) TextDocumentWillSave(logger jsonrpc.FunctionLogger, params *lsp.WillSaveTextDocumentParams) {
	panic("unimplemented")
}

func (server *IDELSPServer) TextDocumentDidSave(logger jsonrpc.FunctionLogger, params *lsp.DidSaveTextDocumentParams) {
	server.ls.TextDocumentDidSaveNotifFromIDE(logger, params)
}

func (server *IDELSPServer) TextDocumentDidClose(logger jsonrpc.FunctionLogger, params *lsp.DidCloseTextDocumentParams) {
	server.ls.TextDocumentDidCloseNotifFromIDE(logger, params)
}
