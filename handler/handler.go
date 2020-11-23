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

	"github.com/arduino/arduino-cli/arduino/builder"
	"github.com/arduino/arduino-cli/executils"
	"github.com/arduino/go-paths-helper"
	"github.com/bcmi-labs/arduino-language-server/handler/sourcemapper"
	"github.com/bcmi-labs/arduino-language-server/handler/textutils"
	"github.com/bcmi-labs/arduino-language-server/streams"
	"github.com/pkg/errors"
	lsp "github.com/sourcegraph/go-lsp"
	"github.com/sourcegraph/jsonrpc2"
)

var globalCliPath string
var globalClangdPath string
var enableLogging bool
var asyncProcessing bool

// Setup initializes global variables.
func Setup(cliPath string, clangdPath string, _enableLogging bool, _asyncProcessing bool) {
	globalCliPath = cliPath
	globalClangdPath = clangdPath
	enableLogging = _enableLogging
	asyncProcessing = _asyncProcessing
}

// CLangdStarter starts clangd and returns its stdin/out/err
type CLangdStarter func() (stdin io.WriteCloser, stdout io.ReadCloser, stderr io.ReadCloser)

// NewInoHandler creates and configures an InoHandler.
func NewInoHandler(stdio io.ReadWriteCloser, board Board) *InoHandler {
	handler := &InoHandler{
		data:         map[lsp.DocumentURI]*FileData{},
		trackedFiles: map[lsp.DocumentURI]lsp.TextDocumentItem{},
		config: BoardConfig{
			SelectedBoard: board,
		},
	}

	stdStream := jsonrpc2.NewBufferedStream(stdio, jsonrpc2.VSCodeObjectCodec{})
	var stdHandler jsonrpc2.Handler = jsonrpc2.HandlerWithError(handler.HandleMessageFromIDE)
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
	StdioConn               *jsonrpc2.Conn
	ClangdConn              *jsonrpc2.Conn
	buildPath               *paths.Path
	buildSketchRoot         *paths.Path
	buildSketchCpp          *paths.Path
	sketchRoot              *paths.Path
	sketchName              string
	sketchMapper            *sourcemapper.InoMapper
	sketchTrackedFilesCount int
	trackedFiles            map[lsp.DocumentURI]lsp.TextDocumentItem

	data         map[lsp.DocumentURI]*FileData
	config       BoardConfig
	synchronizer Synchronizer
}

// FileData gathers information on a .ino source file.
type FileData struct {
	sourceText string
	sourceURI  lsp.DocumentURI
	targetURI  lsp.DocumentURI
	sourceMap  *sourcemapper.InoMapper
	version    int
}

// StopClangd closes the connection to the clangd process.
func (handler *InoHandler) StopClangd() {
	handler.ClangdConn.Close()
	handler.ClangdConn = nil
}

// HandleMessageFromIDE handles a message received from the IDE client (via stdio).
func (handler *InoHandler) HandleMessageFromIDE(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error) {
	needsWriteLock := map[string]bool{
		"textDocument/didOpen":   true,
		"textDocument/didChange": true,
		"textDocument/didClose":  true,
	}
	if needsWriteLock[req.Method] {
		// handler.synchronizer.DataMux.Lock()
		// defer handler.synchronizer.DataMux.Unlock()
	} else {
		// handler.synchronizer.DataMux.RLock()
		// defer handler.synchronizer.DataMux.RUnlock()
	}

	// Handle LSP methods: transform parameters and send to clangd
	var uri lsp.DocumentURI

	params, err := readParams(req.Method, req.Params)
	if err != nil {
		return nil, err
	}
	if params == nil {
		params = req.Params
	}
	switch p := params.(type) {
	case *lsp.InitializeParams:
		// method "initialize"
		handler.synchronizer.DataMux.RLock()
		err = handler.initializeWorkbench(ctx, p)
		handler.synchronizer.DataMux.RUnlock()

	case *lsp.DidOpenTextDocumentParams:
		// method "textDocument/didOpen":
		uri = p.TextDocument.URI
		handler.synchronizer.DataMux.Lock()
		res, err := handler.didOpen(ctx, p)
		handler.synchronizer.DataMux.Unlock()
		if res == nil {
			log.Println("    notification is not propagated to clangd")
			return nil, err // do not propagate to clangd
		}
		params = res

	case *lsp.CompletionParams: // "textDocument/completion":
		uri = p.TextDocument.URI
		log.Printf("--> completion(%s:%d:%d)\n", p.TextDocument.URI, p.Position.Line, p.Position.Character)

		err = handler.ino2cppTextDocumentPositionParams(&p.TextDocumentPositionParams)
		log.Printf("    --> completion(%s:%d:%d)\n", p.TextDocument.URI, p.Position.Line, p.Position.Character)

	case *lsp.DidChangeTextDocumentParams: // "textDocument/didChange":
		uri = p.TextDocument.URI
		err = handler.ino2cppDidChangeTextDocumentParams(ctx, p)
	case *lsp.DidSaveTextDocumentParams: // "textDocument/didSave":
		uri = p.TextDocument.URI
		err = handler.sketchToBuildPathTextDocumentIdentifier(&p.TextDocument)
	case *lsp.DidCloseTextDocumentParams: // "textDocument/didClose":
		uri = p.TextDocument.URI
		err = handler.sketchToBuildPathTextDocumentIdentifier(&p.TextDocument)
		handler.deleteFileData(uri)
	case *lsp.CodeActionParams: // "textDocument/codeAction":
		uri = p.TextDocument.URI
		err = handler.ino2cppCodeActionParams(p)
	// case "textDocument/signatureHelp":
	// 	fallthrough
	// case "textDocument/hover":
	// 	fallthrough
	// case "textDocument/definition":
	// 	fallthrough
	// case "textDocument/typeDefinition":
	// 	fallthrough
	// case "textDocument/implementation":
	// 	fallthrough
	case *lsp.TextDocumentPositionParams: // "textDocument/documentHighlight":
		uri = p.TextDocument.URI
		err = handler.ino2cppTextDocumentPositionParams(p)
	case *lsp.ReferenceParams: // "textDocument/references":
		uri = p.TextDocument.URI
		err = handler.ino2cppTextDocumentPositionParams(&p.TextDocumentPositionParams)
	case *lsp.DocumentFormattingParams: // "textDocument/formatting":
		uri = p.TextDocument.URI
		err = handler.sketchToBuildPathTextDocumentIdentifier(&p.TextDocument)
	case *lsp.DocumentRangeFormattingParams: // "textDocument/rangeFormatting":
		uri = p.TextDocument.URI
		err = handler.ino2cppDocumentRangeFormattingParams(p)
	case *lsp.DocumentOnTypeFormattingParams: // "textDocument/onTypeFormatting":
		uri = p.TextDocument.URI
		err = handler.ino2cppDocumentOnTypeFormattingParams(p)
	case *lsp.DocumentSymbolParams: // "textDocument/documentSymbol":
		uri = p.TextDocument.URI
		err = handler.sketchToBuildPathTextDocumentIdentifier(&p.TextDocument)
	case *lsp.RenameParams: // "textDocument/rename":
		uri = p.TextDocument.URI
		err = handler.ino2cppRenameParams(p)
	case *lsp.DidChangeWatchedFilesParams: // "workspace/didChangeWatchedFiles":
		err = handler.ino2cppDidChangeWatchedFilesParams(p)
	case *lsp.ExecuteCommandParams: // "workspace/executeCommand":
		err = handler.ino2cppExecuteCommand(p)
	}
	if err != nil {
		log.Printf("    ~~~ %s", err)
		return nil, err
	}

	var result interface{}
	if req.Notif {
		err = handler.ClangdConn.Notify(ctx, req.Method, params)
		if enableLogging {
			log.Println("    sent", req.Method, "notification to clangd")
		}
	} else {
		ctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
		defer cancel()
		result, err = sendRequest(ctx, handler.ClangdConn, req.Method, params)
		if enableLogging {
			log.Println("    sent", req.Method, "request id", req.ID, " to clangd")
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
	}

	// Transform and return the result
	if result != nil {
		result = handler.transformClangdResult(req.Method, uri, result)
	}
	return result, err
}

func (handler *InoHandler) exit() {
	log.Println("Please restart the language server.")
	handler.StopClangd()
	os.Exit(1)
}

func newPathFromURI(uri lsp.DocumentURI) *paths.Path {
	return paths.New(uriToPath(uri))
}

func (handler *InoHandler) initializeWorkbench(ctx context.Context, params *lsp.InitializeParams) error {
	rootURI := params.RootURI
	log.Printf("--> initializeWorkbench(%s)\n", rootURI)

	handler.sketchRoot = newPathFromURI(rootURI)
	handler.sketchName = handler.sketchRoot.Base()
	if buildPath, err := generateBuildEnvironment(handler.sketchRoot, handler.config.SelectedBoard.Fqbn); err == nil {
		handler.buildPath = buildPath
		handler.buildSketchRoot = buildPath.Join("sketch")
	} else {
		return err
	}
	handler.buildSketchCpp = handler.buildSketchRoot.Join(handler.sketchName + ".ino.cpp")

	if cppContent, err := handler.buildSketchCpp.ReadFile(); err == nil {
		handler.sketchMapper = sourcemapper.CreateInoMapper(bytes.NewReader(cppContent))
	} else {
		return errors.WithMessage(err, "reading generated cpp file from sketch")
	}

	clangdStdout, clangdStdin, clangdStderr := startClangd(handler.buildPath, handler.buildSketchCpp)
	clangdStdio := streams.NewReadWriteCloser(clangdStdin, clangdStdout)
	if enableLogging {
		clangdStdio = streams.LogReadWriteCloserAs(clangdStdio, "inols-clangd.log")
		go io.Copy(streams.OpenLogFileAs("inols-clangd-err.log"), clangdStderr)
	} else {
		go io.Copy(os.Stderr, clangdStderr)
	}

	clangdStream := jsonrpc2.NewBufferedStream(clangdStdio, jsonrpc2.VSCodeObjectCodec{})
	clangdHandler := jsonrpc2.AsyncHandler(jsonrpc2.HandlerWithError(handler.FromClangd))
	handler.ClangdConn = jsonrpc2.NewConn(context.Background(), clangdStream, clangdHandler)

	params.RootPath = handler.buildSketchRoot.String()
	params.RootURI = pathToURI(handler.buildSketchRoot.String())
	return nil
}

func startClangd(compileCommandsDir, sketchCpp *paths.Path) (io.WriteCloser, io.ReadCloser, io.ReadCloser) {
	// Open compile_commands.json and find the main cross-compiler executable
	compileCommands, err := builder.LoadCompilationDatabase(compileCommandsDir.Join("compile_commands.json"))
	if err != nil {
		panic("could not find compile_commands.json")
	}
	compilers := map[string]bool{}
	for _, cmd := range compileCommands.Contents {
		if len(cmd.Arguments) == 0 {
			panic("invalid empty argument field in compile_commands.json")
		}
		compilers[cmd.Arguments[0]] = true
	}
	if len(compilers) == 0 {
		panic("main compiler not found")
	}

	// Start clangd
	args := []string{
		globalClangdPath,
		"-log=verbose",
		fmt.Sprintf(`--compile-commands-dir=%s`, compileCommandsDir),
	}
	for compiler := range compilers {
		args = append(args, fmt.Sprintf("-query-driver=%s", compiler))
	}
	if enableLogging {
		log.Println("    Starting clangd:", strings.Join(args, " "))
	}
	if clangdCmd, err := executils.NewProcess(args...); err != nil {
		panic("starting clangd: " + err.Error())
	} else if clangdIn, err := clangdCmd.StdinPipe(); err != nil {
		panic("getting clangd stdin: " + err.Error())
	} else if clangdOut, err := clangdCmd.StdoutPipe(); err != nil {
		panic("getting clangd stdout: " + err.Error())
	} else if clangdErr, err := clangdCmd.StderrPipe(); err != nil {
		panic("getting clangd stderr: " + err.Error())
	} else if err := clangdCmd.Start(); err != nil {
		panic("running clangd: " + err.Error())
	} else {
		return clangdIn, clangdOut, clangdErr
	}
}

func (handler *InoHandler) didOpen(ctx context.Context, params *lsp.DidOpenTextDocumentParams) (*lsp.DidOpenTextDocumentParams, error) {
	// Add the TextDocumentItem in the tracked files list
	doc := params.TextDocument
	handler.trackedFiles[doc.URI] = doc
	log.Printf("--> didOpen(%s)", doc.URI)

	// If we are tracking a .ino...
	if newPathFromURI(doc.URI).Ext() == ".ino" {
		handler.sketchTrackedFilesCount++
		log.Printf("    increasing .ino tracked files count: %d", handler.sketchTrackedFilesCount)

		// ...notify clang that sketchCpp is no longer valid on disk
		if handler.sketchTrackedFilesCount == 1 {
			sketchCpp, err := handler.buildSketchCpp.ReadFile()
			newParam := &lsp.DidOpenTextDocumentParams{
				TextDocument: lsp.TextDocumentItem{
					URI:        pathToURI(handler.buildSketchCpp.String()),
					Text:       string(sketchCpp),
					LanguageID: "cpp",
					Version:    1,
				},
			}
			log.Printf("    message for clangd: didOpen(%s)", newParam.TextDocument.URI)
			return newParam, err
		}
	}
	return nil, nil
}

func (handler *InoHandler) updateFileData(ctx context.Context, data *FileData, change *lsp.TextDocumentContentChangeEvent) (err error) {
	rang := change.Range
	if rang == nil || rang.Start.Line != rang.End.Line {
		// Update the source text and regenerate the cpp code
		var newSourceText string
		if rang == nil {
			newSourceText = change.Text
		} else {
			newSourceText, err = textutils.ApplyTextChange(data.sourceText, *rang, change.Text)
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
				data.sourceText = newSourceText
				data.sourceMap.Update(rang.End.Line-rang.Start.Line, rang.Start.Line, change.Text)
				*rang = data.sourceMap.InoToCppLSPRange(data.sourceURI, *rang)
				return nil
			}
		}

		data.sourceText = newSourceText
		data.sourceMap = sourcemapper.CreateInoMapper(bytes.NewReader(targetBytes))

		change.Text = string(targetBytes)
		change.Range = nil
		change.RangeLength = 0
	} else {
		// Apply an update to a single line both to the source and the target text
		data.sourceText, err = textutils.ApplyTextChange(data.sourceText, *rang, change.Text)
		if err != nil {
			return err
		}
		data.sourceMap.Update(0, rang.Start.Line, change.Text)

		*rang = data.sourceMap.InoToCppLSPRange(data.sourceURI, *rang)
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

func (handler *InoHandler) sketchToBuildPathTextDocumentIdentifier(doc *lsp.TextDocumentIdentifier) error {
	// Sketchbook/Sketch/Sketch.ino      -> build-path/sketch/Sketch.ino.cpp
	// Sketchbook/Sketch/AnotherTab.ino  -> build-path/sketch/Sketch.ino.cpp  (different section from above)
	// Sketchbook/Sketch/AnotherFile.cpp -> build-path/sketch/AnotherFile.cpp (1:1)
	// another/path/source.cpp           -> unchanged

	// Convert sketch path to build path
	docFile := newPathFromURI(doc.URI)
	newDocFile := docFile

	if docFile.Ext() == ".ino" {
		newDocFile = handler.buildSketchCpp
	} else if inside, err := docFile.IsInsideDir(handler.sketchRoot); err != nil {
		log.Printf("    could not determine if '%s' is inside '%s'", docFile, handler.sketchRoot)
		return unknownURI(doc.URI)
	} else if !inside {
		log.Printf("    passing doc identifier to '%s' as-is", docFile)
	} else if rel, err := handler.sketchRoot.RelTo(docFile); err != nil {
		log.Printf("    could not determine rel-path of '%s' in '%s", docFile, handler.sketchRoot)
		return unknownURI(doc.URI)
	} else {
		newDocFile = handler.buildSketchRoot.JoinPath(rel)
	}
	log.Printf("    URI: '%s' -> '%s'", docFile, newDocFile)
	doc.URI = pathToURI(newDocFile.String())
	return nil
}

func (handler *InoHandler) ino2cppDidChangeTextDocumentParams(ctx context.Context, params *lsp.DidChangeTextDocumentParams) error {
	handler.sketchToBuildPathTextDocumentIdentifier(&params.TextDocument.TextDocumentIdentifier)
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
	sourceURI := params.TextDocument.URI
	if strings.HasSuffix(string(sourceURI), ".ino") {
		line, ok := handler.sketchMapper.InoToCppLineOk(sourceURI, params.Position.Line)
		if !ok {
			log.Printf("    invalid line requested: %s:%d", sourceURI, params.Position.Line)
			return unknownURI(params.TextDocument.URI)
		}
		params.Position.Line = line
	}
	handler.sketchToBuildPathTextDocumentIdentifier(&params.TextDocument)
	return nil
}

func (handler *InoHandler) ino2cppCodeActionParams(params *lsp.CodeActionParams) error {
	handler.sketchToBuildPathTextDocumentIdentifier(&params.TextDocument)
	if data, ok := handler.data[params.TextDocument.URI]; ok {
		params.Range = data.sourceMap.InoToCppLSPRange(data.sourceURI, params.Range)
		for index := range params.Context.Diagnostics {
			r := &params.Context.Diagnostics[index].Range
			*r = data.sourceMap.InoToCppLSPRange(data.sourceURI, *r)
		}
		return nil
	}
	return unknownURI(params.TextDocument.URI)
}

func (handler *InoHandler) ino2cppDocumentRangeFormattingParams(params *lsp.DocumentRangeFormattingParams) error {
	handler.sketchToBuildPathTextDocumentIdentifier(&params.TextDocument)
	if data, ok := handler.data[params.TextDocument.URI]; ok {
		params.Range = data.sourceMap.InoToCppLSPRange(data.sourceURI, params.Range)
		return nil
	}
	return unknownURI(params.TextDocument.URI)
}

func (handler *InoHandler) ino2cppDocumentOnTypeFormattingParams(params *lsp.DocumentOnTypeFormattingParams) error {
	handler.sketchToBuildPathTextDocumentIdentifier(&params.TextDocument)
	if data, ok := handler.data[params.TextDocument.URI]; ok {
		params.Position.Line = data.sourceMap.InoToCppLine(data.sourceURI, params.Position.Line)
		return nil
	}
	return unknownURI(params.TextDocument.URI)
}

func (handler *InoHandler) ino2cppRenameParams(params *lsp.RenameParams) error {
	handler.sketchToBuildPathTextDocumentIdentifier(&params.TextDocument)
	if data, ok := handler.data[params.TextDocument.URI]; ok {
		params.Position.Line = data.sourceMap.InoToCppLine(data.sourceURI, params.Position.Line)
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
				newValue[index] = lsp.TextEdit{
					NewText: edit[index].NewText,
					Range:   data.sourceMap.InoToCppLSPRange(data.sourceURI, edit[index].Range),
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

	switch r := result.(type) {
	case *lsp.CompletionList: // "textDocument/completion":
		handler.cpp2inoCompletionList(r, uri)
	case *[]*commandOrCodeAction: // "textDocument/codeAction":
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
	case *Hover: // "textDocument/hover":
		if len(r.Contents.Value) == 0 {
			return nil
		}
		handler.cpp2inoHover(r, uri)
	// case "textDocument/definition":
	// 	fallthrough
	// case "textDocument/typeDefinition":
	// 	fallthrough
	// case "textDocument/implementation":
	// 	fallthrough
	case *[]lsp.Location: // "textDocument/references":
		for index := range *r {
			handler.cpp2inoLocation(&(*r)[index])
		}
	case *[]lsp.DocumentHighlight: // "textDocument/documentHighlight":
		for index := range *r {
			handler.cpp2inoDocumentHighlight(&(*r)[index], uri)
		}
	// case "textDocument/formatting":
	// 	fallthrough
	// case "textDocument/rangeFormatting":
	// 	fallthrough
	case *[]lsp.TextEdit: // "textDocument/onTypeFormatting":
		for index := range *r {
			handler.cpp2inoTextEdit(&(*r)[index], uri)
		}
	case *[]*documentSymbolOrSymbolInformation: // "textDocument/documentSymbol":
		if len(*r) == 0 {
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
	case *lsp.WorkspaceEdit: // "textDocument/rename":
		return handler.cpp2inoWorkspaceEdit(r)
	case *[]lsp.SymbolInformation: // "workspace/symbol":
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
					_, item.TextEdit.Range = data.sourceMap.CppToInoRange(item.TextEdit.Range)
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
			_, codeAction.Diagnostics[index].Range = data.sourceMap.CppToInoRange(codeAction.Diagnostics[index].Range)
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
				_, newRange := data.sourceMap.CppToInoRange(edit[index].Range)
				newValue[index] = lsp.TextEdit{
					NewText: edit[index].NewText,
					Range:   newRange,
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
			_, *r = data.sourceMap.CppToInoRange(*r)
		}
	}
}

func (handler *InoHandler) cpp2inoLocation(location *lsp.Location) {
	if data, ok := handler.data[location.URI]; ok {
		location.URI = data.sourceURI
		_, location.Range = data.sourceMap.CppToInoRange(location.Range)
	}
}

func (handler *InoHandler) cpp2inoDocumentHighlight(highlight *lsp.DocumentHighlight, uri lsp.DocumentURI) {
	if data, ok := handler.data[uri]; ok {
		_, highlight.Range = data.sourceMap.CppToInoRange(highlight.Range)
	}
}

func (handler *InoHandler) cpp2inoTextEdit(edit *lsp.TextEdit, uri lsp.DocumentURI) {
	if data, ok := handler.data[uri]; ok {
		_, edit.Range = data.sourceMap.CppToInoRange(edit.Range)
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
		_, symbol.Range = data.sourceMap.CppToInoRange(symbol.Range)

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

		_, symbol.SelectionRange = data.sourceMap.CppToInoRange(symbol.SelectionRange)
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
			if _, startLine, ok := data.sourceMap.CppToInoLineOk(r.Start.Line); ok {
				r.Start.Line = startLine
				_, r.End.Line = data.sourceMap.CppToInoLine(r.End.Line)
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
