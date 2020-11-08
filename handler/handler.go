package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/pkg/errors"
	lsp "github.com/sourcegraph/go-lsp"
	"github.com/sourcegraph/jsonrpc2"
)

var globalCliPath string
var enableLogging bool
var asyncProcessing bool

// Setup initializes global variables.
func Setup(cliPath string, _enableLogging bool, _asyncProcessing bool) {
	globalCliPath = cliPath
	enableLogging = _enableLogging
	asyncProcessing = _asyncProcessing
}

// CLangdStarter starts clangd and returns its stdin/out/err
type CLangdStarter func() (stdin io.WriteCloser, stdout io.ReadCloser, stderr io.ReadCloser)

// NewInoHandler creates and configures an InoHandler.
func NewInoHandler(stdio io.ReadWriteCloser, clangdStdio io.ReadWriteCloser, board Board) *InoHandler {
	handler := &InoHandler{
		data: make(map[lsp.DocumentURI]*FileData),
		config: BoardConfig{
			SelectedBoard: board,
		},
	}

	clangdStream := jsonrpc2.NewBufferedStream(clangdStdio, jsonrpc2.VSCodeObjectCodec{})
	clangdHandler := jsonrpc2.AsyncHandler(jsonrpc2.HandlerWithError(handler.FromClangd))
	handler.ClangdConn = jsonrpc2.NewConn(context.Background(), clangdStream, clangdHandler)

	stdStream := jsonrpc2.NewBufferedStream(stdio, jsonrpc2.VSCodeObjectCodec{})
	var stdHandler jsonrpc2.Handler = jsonrpc2.HandlerWithError(handler.FromStdio)
	if asyncProcessing {
		stdHandler = AsyncHandler{
			handler:      stdHandler,
			synchronizer: &handler.synchronizer,
		}
	}
	handler.StdioConn = jsonrpc2.NewConn(context.Background(), stdStream, stdHandler)
	if enableLogging {
		log.Println("Initial board configuration:", board)
	}
	return handler
}

// InoHandler is a JSON-RPC handler that delegates messages to clangd.
type InoHandler struct {
	StdioConn    *jsonrpc2.Conn
	ClangdConn   *jsonrpc2.Conn
	data         map[lsp.DocumentURI]*FileData
	config       BoardConfig
	synchronizer Synchronizer
}

// FileData gathers information on a .ino source file.
type FileData struct {
	sourceText    string
	sourceURI     lsp.DocumentURI
	targetURI     lsp.DocumentURI
	sourceLineMap map[int]int
	targetLineMap map[int]int
	version       int
}

// StopClangd closes the connection to the clangd process.
func (handler *InoHandler) StopClangd() {
	handler.ClangdConn.Close()
	handler.ClangdConn = nil
}

// FromStdio handles a message received from the client (via stdio).
func (handler *InoHandler) FromStdio(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	params, err := readParams(req.Method, req.Params)
	if err != nil {
		return
	}

	// Handle LSP methods: transform parameters and send to clangd
	var uri lsp.DocumentURI
	if params == nil {
		params = req.Params
	} else {
		uri, err = handler.transformParamsToClangd(ctx, req.Method, params)
	}
	if err != nil {
		return
	}
	if req.Notif {
		err = handler.ClangdConn.Notify(ctx, req.Method, params)
		if enableLogging {
			log.Println("From stdio:", req.Method)
		}
	} else {
		ctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
		defer cancel()
		result, err = sendRequest(ctx, handler.ClangdConn, req.Method, params)
		if enableLogging {
			log.Println("From stdio:", req.Method, "id", req.ID)
		}
	}
	if err != nil {
		// Exit the process and trigger a restart by the client in case of a severe error
		if err.Error() == "context deadline exceeded" {
			log.Println("Timeout exceeded while waiting for a reply from clangd.")
			handler.exit()
		}
		if strings.Contains(err.Error(), "non-added document") || strings.Contains(err.Error(), "non-added file") {
			log.Println("The clangd process has lost track of the open document.")
			handler.exit()
		}
		return
	}

	// Transform and return the result
	if result != nil {
		result = handler.transformClangdResult(req.Method, uri, result)
	}
	return
}

func (handler *InoHandler) exit() {
	log.Println("Please restart the language server.")
	handler.StopClangd()
	os.Exit(1)
}

func (handler *InoHandler) transformParamsToClangd(ctx context.Context, method string, params interface{}) (uri lsp.DocumentURI, err error) {
	needsWriteLock := method == "textDocument/didOpen" || method == "textDocument/didChange" || method == "textDocument/didClose"
	if needsWriteLock {
		handler.synchronizer.DataMux.Lock()
		defer handler.synchronizer.DataMux.Unlock()
	} else {
		handler.synchronizer.DataMux.RLock()
		defer handler.synchronizer.DataMux.RUnlock()
	}

	switch method {
	case "textDocument/didOpen":
		p := params.(*lsp.DidOpenTextDocumentParams)
		uri = p.TextDocument.URI
		err = handler.ino2cppTextDocumentItem(ctx, &p.TextDocument)
	case "textDocument/didChange":
		p := params.(*lsp.DidChangeTextDocumentParams)
		uri = p.TextDocument.URI
		err = handler.ino2cppDidChangeTextDocumentParams(ctx, p)
	case "textDocument/didSave":
		p := params.(*lsp.DidSaveTextDocumentParams)
		uri = p.TextDocument.URI
		err = handler.ino2cppTextDocumentIdentifier(&p.TextDocument)
	case "textDocument/didClose":
		p := params.(*lsp.DidCloseTextDocumentParams)
		uri = p.TextDocument.URI
		err = handler.ino2cppTextDocumentIdentifier(&p.TextDocument)
		handler.deleteFileData(uri)
	case "textDocument/completion":
		p := params.(*lsp.CompletionParams)
		uri = p.TextDocument.URI
		err = handler.ino2cppTextDocumentPositionParams(&p.TextDocumentPositionParams)
	case "textDocument/codeAction":
		p := params.(*lsp.CodeActionParams)
		uri = p.TextDocument.URI
		err = handler.ino2cppCodeActionParams(p)
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
		p := params.(*lsp.TextDocumentPositionParams)
		uri = p.TextDocument.URI
		err = handler.ino2cppTextDocumentPositionParams(p)
	case "textDocument/references":
		p := params.(*lsp.ReferenceParams)
		uri = p.TextDocument.URI
		err = handler.ino2cppTextDocumentPositionParams(&p.TextDocumentPositionParams)
	case "textDocument/formatting":
		p := params.(*lsp.DocumentFormattingParams)
		uri = p.TextDocument.URI
		err = handler.ino2cppTextDocumentIdentifier(&p.TextDocument)
	case "textDocument/rangeFormatting":
		p := params.(*lsp.DocumentRangeFormattingParams)
		uri = p.TextDocument.URI
		err = handler.ino2cppDocumentRangeFormattingParams(p)
	case "textDocument/onTypeFormatting":
		p := params.(*lsp.DocumentOnTypeFormattingParams)
		uri = p.TextDocument.URI
		err = handler.ino2cppDocumentOnTypeFormattingParams(p)
	case "textDocument/documentSymbol":
		p := params.(*lsp.DocumentSymbolParams)
		uri = p.TextDocument.URI
		err = handler.ino2cppTextDocumentIdentifier(&p.TextDocument)
	case "textDocument/rename":
		p := params.(*lsp.RenameParams)
		uri = p.TextDocument.URI
		err = handler.ino2cppRenameParams(p)
	case "workspace/didChangeWatchedFiles":
		p := params.(*lsp.DidChangeWatchedFilesParams)
		err = handler.ino2cppDidChangeWatchedFilesParams(p)
	case "workspace/executeCommand":
		p := params.(*lsp.ExecuteCommandParams)
		err = handler.ino2cppExecuteCommand(p)
	}
	return
}

func (handler *InoHandler) createFileData(ctx context.Context, sourceURI lsp.DocumentURI, sourceText string, version int) (*FileData, []byte, error) {
	sourcePath := uriToPath(sourceURI)
	targetPath, targetBytes, err := generateCpp([]byte(sourceText), sourcePath, handler.config.SelectedBoard.Fqbn)
	if err != nil {
		err = handler.handleError(ctx, err)
		if len(targetPath) == 0 {
			return nil, nil, err
		}
		// Fallback: use the source text unchanged
		targetBytes, err = copyIno2Cpp(sourceText, targetPath)
		if err != nil {
			return nil, nil, err
		}
	}

	targetURI := pathToURI(targetPath)
	sourceLineMap, targetLineMap := createSourceMaps(bytes.NewReader(targetBytes))
	data := &FileData{
		sourceText,
		sourceURI,
		targetURI,
		sourceLineMap,
		targetLineMap,
		version,
	}
	handler.data[sourceURI] = data
	handler.data[targetURI] = data
	return data, targetBytes, nil
}

func (handler *InoHandler) updateFileData(ctx context.Context, data *FileData, change *lsp.TextDocumentContentChangeEvent) (err error) {
	rang := change.Range
	if rang == nil || rang.Start.Line != rang.End.Line {
		// Update the source text and regenerate the cpp code
		var newSourceText string
		if rang == nil {
			newSourceText = change.Text
		} else {
			newSourceText, err = applyTextChange(data.sourceText, *rang, change.Text)
			if err != nil {
				return err
			}
		}
		targetBytes, err := updateCpp([]byte(newSourceText), uriToPath(data.sourceURI), handler.config.SelectedBoard.Fqbn, false, uriToPath(data.targetURI))
		if err != nil {
			if rang == nil {
				// Fallback: use the source text unchanged
				targetBytes, err = copyIno2Cpp(newSourceText, uriToPath(data.targetURI))
				if err != nil {
					return err
				}
			} else {
				// Fallback: try to apply a multi-line update
				targetStartLine := data.targetLineMap[rang.Start.Line]
				targetEndLine := data.targetLineMap[rang.End.Line]
				data.sourceText = newSourceText
				updateSourceMaps(data.sourceLineMap, data.targetLineMap, rang.End.Line-rang.Start.Line, rang.Start.Line, change.Text)
				rang.Start.Line = targetStartLine
				rang.End.Line = targetEndLine
				return nil
			}
		}

		sourceLineMap, targetLineMap := createSourceMaps(bytes.NewReader(targetBytes))
		data.sourceText = newSourceText
		data.sourceLineMap = sourceLineMap
		data.targetLineMap = targetLineMap

		change.Text = string(targetBytes)
		change.Range = nil
		change.RangeLength = 0
	} else {
		// Apply an update to a single line both to the source and the target text
		targetLine := data.targetLineMap[rang.Start.Line]
		data.sourceText, err = applyTextChange(data.sourceText, *rang, change.Text)
		if err != nil {
			return err
		}
		updateSourceMaps(data.sourceLineMap, data.targetLineMap, 0, rang.Start.Line, change.Text)

		rang.Start.Line = targetLine
		rang.End.Line = targetLine
	}
	return nil
}

func (handler *InoHandler) deleteFileData(sourceURI lsp.DocumentURI) {
	if data, ok := handler.data[sourceURI]; ok {
		delete(handler.data, data.sourceURI)
		delete(handler.data, data.targetURI)
	}
}

func (handler *InoHandler) handleError(ctx context.Context, err error) error {
	errorStr := err.Error()
	var message string
	if strings.Contains(errorStr, "#error") {
		exp, regexpErr := regexp.Compile("#error \"(.*)\"")
		if regexpErr != nil {
			panic(regexpErr)
		}
		submatch := exp.FindStringSubmatch(errorStr)
		message = submatch[1]
	} else if strings.Contains(errorStr, "platform not installed") || strings.Contains(errorStr, "no FQBN provided") {
		if len(handler.config.SelectedBoard.Name) > 0 {
			board := handler.config.SelectedBoard.Name
			message = "Editor support may be inaccurate because the core for the board `" + board + "` is not installed."
			message += " Use the Boards Manager to install it."
		} else {
			// This case happens most often when the app is started for the first time and no
			// board is selected yet. Don't bother the user with an error then.
			return err
		}
	} else if strings.Contains(errorStr, "No such file or directory") {
		exp, regexpErr := regexp.Compile("([\\w\\.\\-]+): No such file or directory")
		if regexpErr != nil {
			panic(regexpErr)
		}
		submatch := exp.FindStringSubmatch(errorStr)
		message = "Editor support may be inaccurate because the header `" + submatch[1] + "` was not found."
		message += " If it is part of a library, use the Library Manager to install it."
	} else {
		message = "Could not start editor support.\n" + errorStr
	}
	go handler.showMessage(ctx, lsp.MTError, message)
	return errors.New(message)
}

func (handler *InoHandler) ino2cppTextDocumentIdentifier(doc *lsp.TextDocumentIdentifier) error {
	if data, ok := handler.data[doc.URI]; ok {
		doc.URI = data.targetURI
		return nil
	}
	return unknownURI(doc.URI)
}

func (handler *InoHandler) ino2cppTextDocumentItem(ctx context.Context, doc *lsp.TextDocumentItem) error {
	if strings.HasSuffix(string(doc.URI), ".ino") {
		data, targetBytes, err := handler.createFileData(ctx, doc.URI, doc.Text, doc.Version)
		if err != nil {
			return err
		}
		doc.LanguageID = "cpp"
		doc.URI = data.targetURI
		doc.Text = string(targetBytes)
	}
	return nil
}

func (handler *InoHandler) ino2cppDidChangeTextDocumentParams(ctx context.Context, params *lsp.DidChangeTextDocumentParams) error {
	handler.ino2cppTextDocumentIdentifier(&params.TextDocument.TextDocumentIdentifier)
	if data, ok := handler.data[params.TextDocument.URI]; ok {
		for index := range params.ContentChanges {
			err := handler.updateFileData(ctx, data, &params.ContentChanges[index])
			if err != nil {
				return err
			}
		}
		data.version = params.TextDocument.Version
		return nil
	}
	return unknownURI(params.TextDocument.URI)
}

func (handler *InoHandler) ino2cppTextDocumentPositionParams(params *lsp.TextDocumentPositionParams) error {
	handler.ino2cppTextDocumentIdentifier(&params.TextDocument)
	if data, ok := handler.data[params.TextDocument.URI]; ok {
		targetLine := data.targetLineMap[params.Position.Line]
		params.Position.Line = targetLine
		return nil
	}
	return unknownURI(params.TextDocument.URI)
}

func (handler *InoHandler) ino2cppCodeActionParams(params *lsp.CodeActionParams) error {
	handler.ino2cppTextDocumentIdentifier(&params.TextDocument)
	if data, ok := handler.data[params.TextDocument.URI]; ok {
		params.Range.Start.Line = data.targetLineMap[params.Range.Start.Line]
		params.Range.End.Line = data.targetLineMap[params.Range.End.Line]
		for index := range params.Context.Diagnostics {
			r := &params.Context.Diagnostics[index].Range
			r.Start.Line = data.targetLineMap[r.Start.Line]
			r.End.Line = data.targetLineMap[r.End.Line]
		}
		return nil
	}
	return unknownURI(params.TextDocument.URI)
}

func (handler *InoHandler) ino2cppDocumentRangeFormattingParams(params *lsp.DocumentRangeFormattingParams) error {
	handler.ino2cppTextDocumentIdentifier(&params.TextDocument)
	if data, ok := handler.data[params.TextDocument.URI]; ok {
		params.Range.Start.Line = data.targetLineMap[params.Range.Start.Line]
		params.Range.End.Line = data.targetLineMap[params.Range.End.Line]
		return nil
	}
	return unknownURI(params.TextDocument.URI)
}

func (handler *InoHandler) ino2cppDocumentOnTypeFormattingParams(params *lsp.DocumentOnTypeFormattingParams) error {
	handler.ino2cppTextDocumentIdentifier(&params.TextDocument)
	if data, ok := handler.data[params.TextDocument.URI]; ok {
		params.Position.Line = data.targetLineMap[params.Position.Line]
		return nil
	}
	return unknownURI(params.TextDocument.URI)
}

func (handler *InoHandler) ino2cppRenameParams(params *lsp.RenameParams) error {
	handler.ino2cppTextDocumentIdentifier(&params.TextDocument)
	if data, ok := handler.data[params.TextDocument.URI]; ok {
		params.Position.Line = data.targetLineMap[params.Position.Line]
		return nil
	}
	return unknownURI(params.TextDocument.URI)
}

func (handler *InoHandler) ino2cppDidChangeWatchedFilesParams(params *lsp.DidChangeWatchedFilesParams) error {
	for index := range params.Changes {
		fileEvent := &params.Changes[index]
		if data, ok := handler.data[fileEvent.URI]; ok {
			fileEvent.URI = data.targetURI
		}
	}
	return nil
}

func (handler *InoHandler) ino2cppExecuteCommand(executeCommand *lsp.ExecuteCommandParams) error {
	if len(executeCommand.Arguments) == 1 {
		arg := handler.parseCommandArgument(executeCommand.Arguments[0])
		if workspaceEdit, ok := arg.(*lsp.WorkspaceEdit); ok {
			executeCommand.Arguments[0] = handler.ino2cppWorkspaceEdit(workspaceEdit)
		}
	}
	return nil
}

func (handler *InoHandler) ino2cppWorkspaceEdit(origEdit *lsp.WorkspaceEdit) *lsp.WorkspaceEdit {
	newEdit := lsp.WorkspaceEdit{Changes: make(map[string][]lsp.TextEdit)}
	for uri, edit := range origEdit.Changes {
		if data, ok := handler.data[lsp.DocumentURI(uri)]; ok {
			newValue := make([]lsp.TextEdit, len(edit))
			for index := range edit {
				r := edit[index].Range
				newValue[index] = lsp.TextEdit{
					NewText: edit[index].NewText,
					Range: lsp.Range{
						Start: lsp.Position{Line: data.targetLineMap[r.Start.Line], Character: r.Start.Character},
						End:   lsp.Position{Line: data.targetLineMap[r.End.Line], Character: r.End.Character},
					},
				}
			}
			newEdit.Changes[string(data.targetURI)] = newValue
		} else {
			newEdit.Changes[uri] = edit
		}
	}
	return &newEdit
}

func (handler *InoHandler) transformClangdResult(method string, uri lsp.DocumentURI, result interface{}) interface{} {
	handler.synchronizer.DataMux.RLock()
	defer handler.synchronizer.DataMux.RUnlock()

	switch method {
	case "textDocument/completion":
		r := result.(*lsp.CompletionList)
		handler.cpp2inoCompletionList(r, uri)
	case "textDocument/codeAction":
		r := result.(*[]*commandOrCodeAction)
		for index := range *r {
			command := (*r)[index].Command
			if command != nil {
				handler.cpp2inoCommand(command)
			}
			codeAction := (*r)[index].CodeAction
			if codeAction != nil {
				handler.cpp2inoCodeAction(codeAction, uri)
			}
		}
	case "textDocument/hover":
		r := result.(*Hover)
		if len(r.Contents.Value) == 0 {
			return nil
		}
		handler.cpp2inoHover(r, uri)
	case "textDocument/definition":
		fallthrough
	case "textDocument/typeDefinition":
		fallthrough
	case "textDocument/implementation":
		fallthrough
	case "textDocument/references":
		r := result.(*[]lsp.Location)
		for index := range *r {
			handler.cpp2inoLocation(&(*r)[index])
		}
	case "textDocument/documentHighlight":
		r := result.(*[]lsp.DocumentHighlight)
		for index := range *r {
			handler.cpp2inoDocumentHighlight(&(*r)[index], uri)
		}
	case "textDocument/formatting":
		fallthrough
	case "textDocument/rangeFormatting":
		fallthrough
	case "textDocument/onTypeFormatting":
		r := result.(*[]lsp.TextEdit)
		for index := range *r {
			handler.cpp2inoTextEdit(&(*r)[index], uri)
		}
	case "textDocument/documentSymbol":
		r, ok := result.(*[]*documentSymbolOrSymbolInformation)

		if !ok || len(*r) == 0 {
			return result
		}

		slice := *r
		if slice[0].DocumentSymbol != nil {
			// Treat the input as []DocumentSymbol
			symbols := make([]DocumentSymbol, len(slice))
			for index := range slice {
				symbols[index] = *slice[index].DocumentSymbol
			}
			return handler.cpp2inoDocumentSymbols(symbols, uri)
		}
		if slice[0].SymbolInformation != nil {
			// Treat the input as []SymbolInformation
			symbols := make([]*lsp.SymbolInformation, len(slice))
			for i, s := range slice {
				symbols[i] = s.SymbolInformation
			}
			return handler.cpp2inoSymbolInformation(symbols)
		}
	case "textDocument/rename":
		r := result.(*lsp.WorkspaceEdit)
		return handler.cpp2inoWorkspaceEdit(r)
	case "workspace/symbol":
		r := result.(*[]lsp.SymbolInformation)
		for index := range *r {
			handler.cpp2inoLocation(&(*r)[index].Location)
		}
	}
	return result
}

func (handler *InoHandler) cpp2inoCompletionList(list *lsp.CompletionList, uri lsp.DocumentURI) {
	if data, ok := handler.data[uri]; ok {
		newItems := make([]lsp.CompletionItem, 0, len(list.Items))
		for _, item := range list.Items {
			if !strings.HasPrefix(item.InsertText, "_") {
				if item.TextEdit != nil {
					r := &item.TextEdit.Range
					r.Start.Line = data.sourceLineMap[r.Start.Line]
					r.End.Line = data.sourceLineMap[r.End.Line]
				}
				newItems = append(newItems, item)
			}
		}
		list.Items = newItems
	}
}

func (handler *InoHandler) cpp2inoCodeAction(codeAction *CodeAction, uri lsp.DocumentURI) {
	codeAction.Edit = handler.cpp2inoWorkspaceEdit(codeAction.Edit)
	if data, ok := handler.data[uri]; ok {
		for index := range codeAction.Diagnostics {
			r := &codeAction.Diagnostics[index].Range
			r.Start.Line = data.sourceLineMap[r.Start.Line]
			r.End.Line = data.sourceLineMap[r.End.Line]
		}
	}
}

func (handler *InoHandler) cpp2inoCommand(command *lsp.Command) {
	if len(command.Arguments) == 1 {
		arg := handler.parseCommandArgument(command.Arguments[0])
		if workspaceEdit, ok := arg.(*lsp.WorkspaceEdit); ok {
			command.Arguments[0] = handler.cpp2inoWorkspaceEdit(workspaceEdit)
		}
	}
}

func (handler *InoHandler) cpp2inoWorkspaceEdit(origEdit *lsp.WorkspaceEdit) *lsp.WorkspaceEdit {
	newEdit := lsp.WorkspaceEdit{Changes: make(map[string][]lsp.TextEdit)}
	for uri, edit := range origEdit.Changes {
		if data, ok := handler.data[lsp.DocumentURI(uri)]; ok {
			newValue := make([]lsp.TextEdit, len(edit))
			for index := range edit {
				r := edit[index].Range
				newValue[index] = lsp.TextEdit{
					NewText: edit[index].NewText,
					Range: lsp.Range{
						Start: lsp.Position{Line: data.sourceLineMap[r.Start.Line], Character: r.Start.Character},
						End:   lsp.Position{Line: data.sourceLineMap[r.End.Line], Character: r.End.Character},
					},
				}
			}
			newEdit.Changes[string(data.sourceURI)] = newValue
		} else {
			newEdit.Changes[uri] = edit
		}
	}
	return &newEdit
}

func (handler *InoHandler) cpp2inoHover(hover *Hover, uri lsp.DocumentURI) {
	if data, ok := handler.data[uri]; ok {
		r := hover.Range
		if r != nil {
			r.Start.Line = data.sourceLineMap[r.Start.Line]
			r.End.Line = data.sourceLineMap[r.End.Line]
		}
	}
}

func (handler *InoHandler) cpp2inoLocation(location *lsp.Location) {
	if data, ok := handler.data[location.URI]; ok {
		location.URI = data.sourceURI
		location.Range.Start.Line = data.sourceLineMap[location.Range.Start.Line]
		location.Range.End.Line = data.sourceLineMap[location.Range.End.Line]
	}
}

func (handler *InoHandler) cpp2inoDocumentHighlight(highlight *lsp.DocumentHighlight, uri lsp.DocumentURI) {
	if data, ok := handler.data[uri]; ok {
		highlight.Range.Start.Line = data.sourceLineMap[highlight.Range.Start.Line]
		highlight.Range.End.Line = data.sourceLineMap[highlight.Range.End.Line]
	}
}

func (handler *InoHandler) cpp2inoTextEdit(edit *lsp.TextEdit, uri lsp.DocumentURI) {
	if data, ok := handler.data[uri]; ok {
		edit.Range.Start.Line = data.sourceLineMap[edit.Range.Start.Line]
		edit.Range.End.Line = data.sourceLineMap[edit.Range.End.Line]
	}
}

func (handler *InoHandler) cpp2inoDocumentSymbols(origSymbols []DocumentSymbol, uri lsp.DocumentURI) []DocumentSymbol {
	data, ok := handler.data[uri]
	if !ok || len(origSymbols) == 0 {
		return origSymbols
	}

	symbolIdx := make(map[string]*DocumentSymbol)
	for i := 0; i < len(origSymbols); i++ {
		symbol := &origSymbols[i]
		symbol.Range.Start.Line = data.sourceLineMap[symbol.Range.Start.Line]
		symbol.Range.End.Line = data.sourceLineMap[symbol.Range.End.Line]

		duplicate := false
		other, duplicate := symbolIdx[symbol.Name]
		if duplicate {
			// We prefer symbols later in the file due to the function header generation. E.g. if one has a function `void foo() {}` somehwre in the code
			// the code generation will add a `void foo();` header at the beginning of the cpp file. We care about the function body later in the file, not
			// the header early on.
			if other.Range.Start.Line < symbol.Range.Start.Line {
				continue
			}
		}

		symbol.SelectionRange.Start.Line = data.sourceLineMap[symbol.SelectionRange.Start.Line]
		symbol.SelectionRange.End.Line = data.sourceLineMap[symbol.SelectionRange.End.Line]
		symbol.Children = handler.cpp2inoDocumentSymbols(symbol.Children, uri)
		symbolIdx[symbol.Name] = symbol
	}

	newSymbols := make([]DocumentSymbol, len(symbolIdx))
	j := 0
	for _, s := range symbolIdx {
		newSymbols[j] = *s
		j++
	}
	return newSymbols
}

func (handler *InoHandler) cpp2inoSymbolInformation(syms []*lsp.SymbolInformation) []lsp.SymbolInformation {
	// Much like in cpp2inoDocumentSymbols we de-duplicate symbols based on file in-file location.
	idx := make(map[string]*lsp.SymbolInformation)
	for _, sym := range syms {
		handler.cpp2inoLocation(&sym.Location)

		nme := fmt.Sprintf("%s::%s", sym.ContainerName, sym.Name)
		other, duplicate := idx[nme]
		if duplicate && other.Location.Range.Start.Line < sym.Location.Range.Start.Line {
			continue
		}

		idx[nme] = sym
	}

	var j int
	symbols := make([]lsp.SymbolInformation, len(idx))
	for _, sym := range idx {
		symbols[j] = *sym
		j++
	}
	return symbols
}

// FromClangd handles a message received from clangd.
func (handler *InoHandler) FromClangd(ctx context.Context, connection *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error) {
	params, _, err := handler.transformParamsToStdio(req.Method, req.Params)
	if err != nil {
		log.Println("From clangd: Method:", req.Method, "Error:", err)
		return nil, err
	}
	var result interface{}
	if req.Notif {
		err = handler.StdioConn.Notify(ctx, req.Method, params)
		if enableLogging {
			log.Println("From clangd:", req.Method)
		}
	} else {
		result, err = sendRequest(ctx, handler.StdioConn, req.Method, params)
		if enableLogging {
			log.Println("From clangd:", req.Method, "id", req.ID)
		}
	}
	return result, err
}

func (handler *InoHandler) transformParamsToStdio(method string, raw *json.RawMessage) (params interface{}, uri lsp.DocumentURI, err error) {
	handler.synchronizer.DataMux.RLock()
	defer handler.synchronizer.DataMux.RUnlock()

	params, err = readParams(method, raw)
	if err != nil {
		return
	} else if params == nil {
		params = raw
		return
	}
	switch method {
	case "textDocument/publishDiagnostics":
		p := params.(*lsp.PublishDiagnosticsParams)
		uri = p.URI
		err = handler.cpp2inoPublishDiagnosticsParams(p)
	case "workspace/applyEdit":
		p := params.(*ApplyWorkspaceEditParams)
		p.Edit = *handler.cpp2inoWorkspaceEdit(&p.Edit)
	}
	return
}

func (handler *InoHandler) cpp2inoPublishDiagnosticsParams(params *lsp.PublishDiagnosticsParams) error {
	if data, ok := handler.data[params.URI]; ok {
		params.URI = data.sourceURI
		newDiagnostics := make([]lsp.Diagnostic, 0, len(params.Diagnostics))
		for index := range params.Diagnostics {
			r := &params.Diagnostics[index].Range
			if startLine, ok := data.sourceLineMap[r.Start.Line]; ok {
				r.Start.Line = startLine
				r.End.Line = data.sourceLineMap[r.End.Line]
				newDiagnostics = append(newDiagnostics, params.Diagnostics[index])
			}
		}
		params.Diagnostics = newDiagnostics
	}
	return nil
}

func (handler *InoHandler) parseCommandArgument(rawArg interface{}) interface{} {
	if m1, ok := rawArg.(map[string]interface{}); ok && len(m1) == 1 && m1["changes"] != nil {
		m2 := m1["changes"].(map[string]interface{})
		workspaceEdit := lsp.WorkspaceEdit{Changes: make(map[string][]lsp.TextEdit)}
		for uri, rawValue := range m2 {
			rawTextEdits := rawValue.([]interface{})
			textEdits := make([]lsp.TextEdit, len(rawTextEdits))
			for index := range rawTextEdits {
				m3 := rawTextEdits[index].(map[string]interface{})
				rawRange := m3["range"]
				m4 := rawRange.(map[string]interface{})
				rawStart := m4["start"]
				m5 := rawStart.(map[string]interface{})
				textEdits[index].Range.Start.Line = int(m5["line"].(float64))
				textEdits[index].Range.Start.Character = int(m5["character"].(float64))
				rawEnd := m4["end"]
				m6 := rawEnd.(map[string]interface{})
				textEdits[index].Range.End.Line = int(m6["line"].(float64))
				textEdits[index].Range.End.Character = int(m6["character"].(float64))
				textEdits[index].NewText = m3["newText"].(string)
			}
			workspaceEdit.Changes[uri] = textEdits
		}
		return &workspaceEdit
	}
	return nil
}

func (handler *InoHandler) showMessage(ctx context.Context, msgType lsp.MessageType, message string) {
	params := lsp.ShowMessageParams{
		Type:    msgType,
		Message: message,
	}
	handler.StdioConn.Notify(ctx, "window/showMessage", &params)
}
