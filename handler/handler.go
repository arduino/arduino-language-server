package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arduino/arduino-cli/arduino/builder"
	"github.com/arduino/arduino-cli/executils"
	"github.com/arduino/arduino-language-server/handler/sourcemapper"
	"github.com/arduino/arduino-language-server/handler/textutils"
	"github.com/arduino/arduino-language-server/streams"
	"github.com/arduino/go-paths-helper"
	"github.com/fatih/color"
	"github.com/pkg/errors"
	"go.bug.st/json"
	"go.bug.st/lsp"
	"go.bug.st/lsp/jsonrpc"
)

var globalCliPath string
var globalCliConfigPath string
var globalClangdPath string
var globalFormatterConf *paths.Path
var enableLogging bool

// Setup initializes global variables.
func Setup(cliPath, cliConfigPath, clangdPath, formatFilePath string, _enableLogging bool) {
	globalCliPath = cliPath
	globalCliConfigPath = cliConfigPath
	globalClangdPath = clangdPath
	if formatFilePath != "" {
		globalFormatterConf = paths.New(formatFilePath)
	}
	enableLogging = _enableLogging
}

// CLangdStarter starts clangd and returns its stdin/out/err
type CLangdStarter func() (stdin io.WriteCloser, stdout io.ReadCloser, stderr io.ReadCloser)

// InoHandler is a JSON-RPC handler that delegates messages to clangd.
type InoHandler struct {
	IDEConn    *jsonrpc.Connection
	ClangdConn *jsonrpc.Connection

	ideMessageCount    int64
	clangdMessageCount int64
	progressHandler    *ProgressProxyHandler

	closing                    chan bool
	clangdStarted              *sync.Cond
	dataMux                    sync.RWMutex
	lspInitializeParams        *lsp.InitializeParams
	buildPath                  *paths.Path
	buildSketchRoot            *paths.Path
	buildSketchCpp             *paths.Path
	buildSketchCppVersion      int
	buildSketchSymbols         []lsp.DocumentSymbol
	buildSketchIncludesCanary  string
	buildSketchSymbolsCanary   string
	buildSketchSymbolsLoad     bool
	buildSketchSymbolsCheck    bool
	rebuildSketchDeadline      *time.Time
	rebuildSketchDeadlineMutex sync.Mutex
	sketchRoot                 *paths.Path
	sketchName                 string
	sketchMapper               *sourcemapper.InoMapper
	sketchTrackedFilesCount    int
	docs                       map[string]*lsp.TextDocumentItem
	inoDocsWithDiagnostics     map[lsp.DocumentURI]bool

	config BoardConfig
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

var yellow = color.New(color.FgHiYellow)

func (handler *InoHandler) writeLock(logger streams.PrefixLogger, requireClangd bool) {
	handler.dataMux.Lock()
	logger(yellow.Sprintf("write-locked"))
	if requireClangd {
		handler.waitClangdStart(logger)
	}
}

func (handler *InoHandler) writeUnlock(logger streams.PrefixLogger) {
	logger(yellow.Sprintf("write-unlocked"))
	handler.dataMux.Unlock()
}

func (handler *InoHandler) readLock(logger streams.PrefixLogger, requireClangd bool) {
	handler.dataMux.RLock()
	logger(yellow.Sprintf("read-locked"))

	for requireClangd && handler.ClangdConn == nil {
		// if clangd is not started...

		// Release the read lock and acquire a write lock
		// (this is required to wait on condition variable and restart clang).
		logger(yellow.Sprintf("clang not started: read-unlocking..."))
		handler.dataMux.RUnlock()

		handler.writeLock(logger, true)
		handler.writeUnlock(logger)

		handler.dataMux.RLock()
		logger(yellow.Sprintf("testing again if clang started: read-locked..."))
	}
}

func (handler *InoHandler) readUnlock(logger streams.PrefixLogger) {
	logger(yellow.Sprintf("read-unlocked"))
	handler.dataMux.RUnlock()
}

func (handler *InoHandler) waitClangdStart(logger streams.PrefixLogger) error {
	if handler.ClangdConn != nil {
		return nil
	}

	logger("(throttled: waiting for clangd)")
	logger(yellow.Sprintf("unlocked (waiting clangd)"))
	handler.clangdStarted.Wait()
	logger(yellow.Sprintf("locked (waiting clangd)"))

	if handler.ClangdConn == nil {
		logger("clangd startup failed: aborting call")
		return errors.New("could not start clangd, aborted")
	}
	return nil
}

// NewInoHandler creates and configures an InoHandler.
func NewInoHandler(stdin io.Reader, stdout io.Writer, board Board) *InoHandler {
	logger := streams.NewPrefixLogger(color.New(color.FgWhite), "LS: ")
	handler := &InoHandler{
		docs:                   map[string]*lsp.TextDocumentItem{},
		inoDocsWithDiagnostics: map[lsp.DocumentURI]bool{},
		closing:                make(chan bool),
		config: BoardConfig{
			SelectedBoard: board,
		},
	}
	handler.clangdStarted = sync.NewCond(&handler.dataMux)

	if buildPath, err := paths.MkTempDir("", "arduino-language-server"); err != nil {
		log.Fatalf("Could not create temp folder: %s", err)
	} else {
		handler.buildPath = buildPath.Canonical()
		handler.buildSketchRoot = handler.buildPath.Join("sketch")
	}
	if enableLogging {
		logger("Initial board configuration: %s", board)
		logger("Language server build path: %s", handler.buildPath)
		logger("Language server build sketch root: %s", handler.buildSketchRoot)
	}

	jsonrpcLogger := streams.NewJsonRPCLogger("IDE", "LS", false)
	handler.IDEConn = jsonrpc.NewConnection(stdin, stdout,
		func(ctx context.Context, method string, params json.RawMessage, respCallback func(result json.RawMessage, err *jsonrpc.ResponseError)) {
			reqLogger, idx := jsonrpcLogger.LogClientRequest(method, params)
			handler.HandleMessageFromIDE(ctx, reqLogger, method, params, func(result json.RawMessage, err *jsonrpc.ResponseError) {
				jsonrpcLogger.LogServerResponse(idx, method, result, err)
				respCallback(result, err)
			})
		},
		func(ctx context.Context, method string, params json.RawMessage) {
			notifLogger := jsonrpcLogger.LogClientNotification(method, params)
			handler.HandleNotificationFromIDE(ctx, notifLogger, method, params)
		},
		func(e error) {},
	)
	go func() {
		handler.IDEConn.Run()
		logger("Lost connection with IDE!")
		handler.Close()
	}()

	handler.progressHandler = NewProgressProxy(handler.IDEConn)

	go handler.rebuildEnvironmentLoop()
	return handler
}

// // FileData gathers information on a .ino source file.
// type FileData struct {
// 	sourceText string
// 	sourceURI  lsp.DocumentURI
// 	targetURI  lsp.DocumentURI
// 	sourceMap  *sourcemapper.InoMapper
// 	version    int
// }

// Close closes all the json-rpc connections.
func (handler *InoHandler) Close() {
	if handler.ClangdConn != nil {
		handler.ClangdConn.Close()
		handler.ClangdConn = nil
	}
	if handler.closing != nil {
		close(handler.closing)
		handler.closing = nil
	}
}

// CloseNotify returns a channel that is closed when the InoHandler is closed
func (handler *InoHandler) CloseNotify() <-chan bool {
	return handler.closing
}

// CleanUp performs cleanup of the workspace and temp files create by the language server
func (handler *InoHandler) CleanUp() {
	if handler.buildPath != nil {
		handler.buildPath.RemoveAll()
		handler.buildPath = nil
	}
}

func (handler *InoHandler) HandleNotificationFromIDE(ctx context.Context, logger streams.PrefixLogger, method string, paramsRaw json.RawMessage) {
	defer streams.CatchAndLogPanic()
	// n := atomic.AddInt64(&handler.ideMessageCount, 1)
	// prefix := fmt.Sprintf("IDE --> %s notif%d ", method, n)

	params, err := lsp.DecodeNotificationParams(method, paramsRaw)
	if err != nil {
		// TODO: log?
		return
	}
	if params == nil {
		// TODO: log?
		return
	}

	// Set up RWLocks and wait for clangd startup
	switch method {
	case "textDocument/didOpen",
		"textDocument/didChange",
		"textDocument/didClose":
		// Write lock - clangd required
		handler.writeLock(logger, true)
		defer handler.writeUnlock(logger)
	case "initialized":
		// Read lock - NO clangd required
		handler.readLock(logger, false)
		defer handler.readUnlock(logger)
	default:
		// Read lock - clangd required
		handler.readLock(logger, true)
		defer handler.readUnlock(logger)
	}

	// Handle LSP methods: transform parameters and send to clangd
	var cppURI lsp.DocumentURI

	switch p := params.(type) {
	case *lsp.InitializedParams:
		// method "initialized"
		logger("notification is not propagated to clangd")
		return // Do not propagate to clangd

	case *lsp.DidOpenTextDocumentParams:
		// method "textDocument/didOpen"
		logger("(%s@%d as '%s')", p.TextDocument.URI, p.TextDocument.Version, p.TextDocument.LanguageID)

		if res, e := handler.didOpen(logger, p); e != nil {
			params = nil
			err = e
		} else if res == nil {
			logger("notification is not propagated to clangd")
			return
		} else {
			logger("to clang: didOpen(%s@%d as '%s')", res.TextDocument.URI, res.TextDocument.Version, res.TextDocument.LanguageID)
			params = res
		}

	case *lsp.DidCloseTextDocumentParams:
		// Method: "textDocument/didClose"
		logger("--> didClose(%s)", p.TextDocument.URI)

		if res, e := handler.didClose(logger, p); e != nil {
		} else if res == nil {
			logger("    --X notification is not propagated to clangd")
			return
		} else {
			logger("    --> didClose(%s)", res.TextDocument.URI)
			params = res
		}

	case *lsp.DidChangeTextDocumentParams:
		// notification "textDocument/didChange"
		logger("--> didChange(%s@%d)", p.TextDocument.URI, p.TextDocument.Version)
		for _, change := range p.ContentChanges {
			logger("     > %s -> %s", change.Range, strconv.Quote(change.Text))
		}

		if res, err := handler.didChange(logger, p); err != nil {
			logger("    --E error: %s", err)
			return
		} else if res == nil {
			logger("    --X notification is not propagated to clangd")
			return
		} else {
			p = res
		}

		logger("    --> didChange(%s@%d)", p.TextDocument.URI, p.TextDocument.Version)
		for _, change := range p.ContentChanges {
			logger("         > %s -> %s", change.Range, strconv.Quote(change.Text))
		}
		if err := handler.ClangdConn.SendNotification(method, lsp.EncodeMessage(p)); err != nil {
			// Exit the process and trigger a restart by the client in case of a severe error
			logger("Connection error with clangd server:")
			logger("%v", err)
			logger("Please restart the language server.")
			handler.Close()
		}
		return

	case *lsp.DidSaveTextDocumentParams:
		// Method: "textDocument/didSave"
		logger("--> %s(%s)", method, p.TextDocument.URI)
		p.TextDocument, err = handler.ino2cppTextDocumentIdentifier(logger, p.TextDocument)
		cppURI = p.TextDocument.URI
		if cppURI.AsPath().EquivalentTo(handler.buildSketchCpp) {
			logger("    --| didSave not forwarded to clangd")
			return
		}
		logger("    --> %s(%s)", method, p.TextDocument.URI)
	}

	if err != nil {
		logger("Error: %s", err)
		return
	}

	logger("sending to Clang")
	if err := handler.ClangdConn.SendNotification(method, lsp.EncodeMessage(params)); err != nil {
		// Exit the process and trigger a restart by the client in case of a severe error
		logger("Connection error with clangd server:")
		logger("vs", err)
		logger("Please restart the language server.")
		handler.Close()
	}
	if handler.buildSketchSymbolsLoad {
		handler.buildSketchSymbolsLoad = false
		handler.buildSketchSymbolsCheck = false
		logger("Queued resfreshing document symbols")
		go handler.LoadCppDocumentSymbols()
	}
	if handler.buildSketchSymbolsCheck {
		handler.buildSketchSymbolsCheck = false
		logger("Queued check document symbols")
		go handler.CheckCppDocumentSymbols()
	}
}

// HandleMessageFromIDE handles a message received from the IDE client (via stdio).
func (handler *InoHandler) HandleMessageFromIDE(ctx context.Context, logger streams.PrefixLogger,
	method string, paramsRaw json.RawMessage,
	returnCB func(result json.RawMessage, err *jsonrpc.ResponseError),
) {
	defer streams.CatchAndLogPanic()
	// n := atomic.AddInt64(&handler.ideMessageCount, 1)
	// prefix := fmt.Sprintf("IDE --> %s %v ", method, n)

	params, err := lsp.DecodeRequestParams(method, paramsRaw)
	if err != nil {
		returnCB(nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInvalidParams, Message: err.Error()})
		return
	}
	if params == nil {
		// TODO: log?
		returnCB(nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInvalidParams})
		return
	}

	// Set up RWLocks and wait for clangd startup
	switch method {
	case "initialize":
		// Write lock - NO clangd required
		handler.writeLock(logger, false)
		defer handler.writeUnlock(logger)
	default:
		// Read lock - clangd required
		handler.readLock(logger, true)
		defer handler.readUnlock(logger)
	}

	// Handle LSP methods: transform parameters and send to clangd
	var inoURI, cppURI lsp.DocumentURI

	switch p := params.(type) {
	case *lsp.InitializeParams:
		// method "initialize"

		go func() {
			defer streams.CatchAndLogPanic()
			logger := streams.NewPrefixLogger(color.New(color.FgCyan), "INIT --- ")
			logger("initializing workbench")

			// Start clangd asynchronously
			handler.writeLock(logger, false) // do not wait for clangd... we are starting it :-)
			defer handler.writeUnlock(logger)

			handler.initializeWorkbench(logger, p)

			// clangd should be running now...
			handler.clangdStarted.Broadcast()

			logger("initializing workbench (done)")
		}()

		returnCB(lsp.EncodeMessage(&lsp.InitializeResult{
			Capabilities: lsp.ServerCapabilities{
				TextDocumentSync: &lsp.TextDocumentSyncOptions{
					OpenClose: true,
					Change:    lsp.TextDocumentSyncKindIncremental,
					Save: &lsp.SaveOptions{
						IncludeText: true,
					},
				},
				HoverProvider: &lsp.HoverOptions{}, // true,
				CompletionProvider: &lsp.CompletionOptions{
					TriggerCharacters: []string{".", "\u003e", ":"},
				},
				SignatureHelpProvider: &lsp.SignatureHelpOptions{
					TriggerCharacters: []string{"(", ","},
				},
				DefinitionProvider: &lsp.DefinitionOptions{}, // true,
				// ReferencesProvider:              &lsp.ReferenceOptions{},  // TODO: true
				DocumentHighlightProvider:       &lsp.DocumentHighlightOptions{}, //true,
				DocumentSymbolProvider:          &lsp.DocumentSymbolOptions{},    //true,
				WorkspaceSymbolProvider:         &lsp.WorkspaceSymbolOptions{},   //true,
				CodeActionProvider:              &lsp.CodeActionOptions{ResolveProvider: true},
				DocumentFormattingProvider:      &lsp.DocumentFormattingOptions{},      //true,
				DocumentRangeFormattingProvider: &lsp.DocumentRangeFormattingOptions{}, //true,
				DocumentOnTypeFormattingProvider: &lsp.DocumentOnTypeFormattingOptions{
					FirstTriggerCharacter: "\n",
				},
				RenameProvider: &lsp.RenameOptions{PrepareProvider: false}, // TODO: true
				ExecuteCommandProvider: &lsp.ExecuteCommandOptions{
					Commands: []string{"clangd.applyFix", "clangd.applyTweak"},
				},
			},
		}), nil)
		return

	case *lsp.CompletionParams:
		// method: "textDocument/completion"
		logger("--> completion(%s:%d:%d)\n", p.TextDocument.URI, p.Position.Line, p.Position.Character)

		if res, e := handler.ino2cppTextDocumentPositionParams(logger, p.TextDocumentPositionParams); e == nil {
			p.TextDocumentPositionParams = res
			logger("    --> completion(%s:%d:%d)\n", p.TextDocument.URI, p.Position.Line, p.Position.Character)
		} else {
			err = e
		}
		inoURI = p.TextDocument.URI

	case *lsp.CodeActionParams:
		// method "textDocument/codeAction"
		inoURI = p.TextDocument.URI
		logger("--> codeAction(%s:%s)", p.TextDocument.URI, p.Range.Start)

		p.TextDocument, err = handler.ino2cppTextDocumentIdentifier(logger, p.TextDocument)
		if err != nil {
			break
		}
		if p.TextDocument.URI.AsPath().EquivalentTo(handler.buildSketchCpp) {
			p.Range = handler.sketchMapper.InoToCppLSPRange(inoURI, p.Range)
			for index := range p.Context.Diagnostics {
				r := &p.Context.Diagnostics[index].Range
				*r = handler.sketchMapper.InoToCppLSPRange(inoURI, *r)
			}
		}
		logger("    --> codeAction(%s:%s)", p.TextDocument.URI, p.Range.Start)

	case *lsp.HoverParams:
		// method: "textDocument/hover"
		doc := p.TextDocumentPositionParams
		logger("--> hover(%s:%d:%d)\n", doc.TextDocument.URI, doc.Position.Line, doc.Position.Character)

		if res, e := handler.ino2cppTextDocumentPositionParams(logger, doc); e == nil {
			p.TextDocumentPositionParams = res
			logger("    --> hover(%s:%d:%d)\n", doc.TextDocument.URI, doc.Position.Line, doc.Position.Character)
		} else {
			err = e
		}
		inoURI = p.TextDocument.URI

	case *lsp.DocumentSymbolParams:
		// method "textDocument/documentSymbol"
		inoURI = p.TextDocument.URI
		logger("--> documentSymbol(%s)", p.TextDocument.URI)

		p.TextDocument, err = handler.ino2cppTextDocumentIdentifier(logger, p.TextDocument)
		logger("    --> documentSymbol(%s)", p.TextDocument.URI)

	case *lsp.DocumentFormattingParams:
		// method "textDocument/formatting"
		inoURI = p.TextDocument.URI
		logger("--> formatting(%s)", p.TextDocument.URI)
		p.TextDocument, err = handler.ino2cppTextDocumentIdentifier(logger, p.TextDocument)
		cppURI = p.TextDocument.URI
		logger("    --> formatting(%s)", p.TextDocument.URI)
		if cleanup, e := handler.createClangdFormatterConfig(logger, cppURI); e != nil {
			err = e
		} else {
			defer cleanup()
		}

	case *lsp.DocumentRangeFormattingParams:
		// Method: "textDocument/rangeFormatting"
		logger("--> %s(%s:%s)", method, p.TextDocument.URI, p.Range)
		inoURI = p.TextDocument.URI
		if cppParams, e := handler.ino2cppDocumentRangeFormattingParams(logger, p); e == nil {
			params = cppParams
			cppURI = cppParams.TextDocument.URI
			logger("    --> %s(%s:%s)", method, cppParams.TextDocument.URI, cppParams.Range)
			if cleanup, e := handler.createClangdFormatterConfig(logger, cppURI); e != nil {
				err = e
			} else {
				defer cleanup()
			}
		} else {
			err = e
		}

	case *lsp.SignatureHelpParams,
		*lsp.DefinitionParams,
		*lsp.TypeDefinitionParams,
		*lsp.ImplementationParams,
		*lsp.DocumentHighlightParams:
		// it was *lsp.TextDocumentPositionParams:

		// Method: "textDocument/signatureHelp"
		// Method: "textDocument/definition"
		// Method: "textDocument/typeDefinition"
		// Method: "textDocument/implementation"
		// Method: "textDocument/documentHighlight"

		tdp := p.(lsp.TextDocumentPositionParams)

		logger("--> %s(%s:%s)", method, tdp.TextDocument.URI, tdp.Position)
		inoURI = tdp.TextDocument.URI
		if res, e := handler.ino2cppTextDocumentPositionParams(logger, tdp); e == nil {
			cppURI = res.TextDocument.URI
			params = res
			logger("    --> %s(%s:%s)", method, res.TextDocument.URI, res.Position)
		} else {
			err = e
		}

	case *lsp.ReferenceParams:
		// "textDocument/references":
		logger("--X " + method)
		return
		inoURI = p.TextDocument.URI
		_, err = handler.ino2cppTextDocumentPositionParams(logger, p.TextDocumentPositionParams)

	case *lsp.DocumentOnTypeFormattingParams:
		// "textDocument/onTypeFormatting":
		logger("--X " + method)
		returnCB(nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesMethodNotFound, Message: "Unimplemented"})
		return
		inoURI = p.TextDocument.URI
		err = handler.ino2cppDocumentOnTypeFormattingParams(p)

	case *lsp.RenameParams:
		// "textDocument/rename":
		logger("--X " + method)
		returnCB(nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesMethodNotFound, Message: "Unimplemented"})
		return
		inoURI = p.TextDocument.URI
		err = handler.ino2cppRenameParams(p)

	case *lsp.DidChangeWatchedFilesParams:
		// "workspace/didChangeWatchedFiles":
		logger("--X " + method)
		returnCB(nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesMethodNotFound, Message: "Unimplemented"})
		return
		err = handler.ino2cppDidChangeWatchedFilesParams(p)

	case *lsp.ExecuteCommandParams:
		// "workspace/executeCommand":
		logger("--X " + method)
		returnCB(nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesMethodNotFound, Message: "Unimplemented"})
		return
		err = handler.ino2cppExecuteCommand(p)
	}
	if err != nil {
		logger("Error: %s", err)
		returnCB(nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()})
		return
	}

	logger("sent to Clang")

	// var result interface{}
	// result, err = lsp.SendRequest(ctx, handler.ClangdConn, method, params)
	clangRawResp, clangErr, err := handler.ClangdConn.SendRequest(ctx, method, lsp.EncodeMessage(params))
	if err != nil {
		// Exit the process and trigger a restart by the client in case of a severe error
		if err.Error() == "context deadline exceeded" {
			logger("Timeout exceeded while waiting for a reply from clangd.")
			logger("Please restart the language server.")
			handler.Close()
		} else if strings.Contains(err.Error(), "non-added document") || strings.Contains(err.Error(), "non-added file") {
			logger("The clangd process has lost track of the open document.")
			logger("%v", err)
			logger("Please restart the language server.")
			handler.Close()
		} else {
			logger("clangd error: %v", err)
		}
		returnCB(nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()})
		return
	}
	if clangErr != nil {
		logger("clangd response error: %v", clangErr.AsError())
		returnCB(nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()})
		return
	}
	clangResp, err := lsp.DecodeResponseResult(method, clangRawResp)
	if err != nil {
		logger("Error decoding clang response: %v", err)
		returnCB(nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()})
		return
	}

	if handler.buildSketchSymbolsLoad {
		handler.buildSketchSymbolsLoad = false
		handler.buildSketchSymbolsCheck = false
		logger("Queued resfreshing document symbols")
		go handler.LoadCppDocumentSymbols()
	}
	if handler.buildSketchSymbolsCheck {
		handler.buildSketchSymbolsCheck = false
		logger("Queued check document symbols")
		go handler.CheckCppDocumentSymbols()
	}

	// Transform and return the result
	if clangResp != nil {
		clangResp = handler.transformClangdResult(logger, method, inoURI, cppURI, clangResp)
	}
	returnCB(lsp.EncodeMessage(clangResp), nil)
}

func (handler *InoHandler) initializeWorkbench(logger streams.PrefixLogger, params *lsp.InitializeParams) error {
	currCppTextVersion := 0
	if params != nil {
		logger("    --> initialize(%s)", params.RootURI)
		handler.lspInitializeParams = params
		handler.sketchRoot = params.RootURI.AsPath()
		handler.sketchName = handler.sketchRoot.Base()
	} else {
		logger("    --> RE-initialize()")
		currCppTextVersion = handler.sketchMapper.CppText.Version
	}

	if err := handler.generateBuildEnvironment(logger, handler.buildPath); err != nil {
		return err
	}
	handler.buildSketchCpp = handler.buildSketchRoot.Join(handler.sketchName + ".ino.cpp")
	handler.buildSketchCppVersion = 1
	handler.lspInitializeParams.RootPath = handler.buildSketchRoot.String()
	handler.lspInitializeParams.RootURI = lsp.NewDocumentURIFromPath(handler.buildSketchRoot)

	if cppContent, err := handler.buildSketchCpp.ReadFile(); err == nil {
		handler.sketchMapper = sourcemapper.CreateInoMapper(cppContent)
		handler.sketchMapper.CppText.Version = currCppTextVersion + 1
	} else {
		return errors.WithMessage(err, "reading generated cpp file from sketch")
	}

	canonicalizeCompileCommandsJSON(handler.buildPath)

	if params == nil {
		// If we are restarting re-synchronize clangd
		cppURI := lsp.NewDocumentURIFromPath(handler.buildSketchCpp)
		cppTextDocumentIdentifier := lsp.TextDocumentIdentifier{URI: cppURI}

		syncEvent := &lsp.DidChangeTextDocumentParams{
			TextDocument: lsp.VersionedTextDocumentIdentifier{
				TextDocumentIdentifier: cppTextDocumentIdentifier,
				Version:                handler.sketchMapper.CppText.Version,
			},
			ContentChanges: []lsp.TextDocumentContentChangeEvent{
				{Text: handler.sketchMapper.CppText.Text}, // Full text change
			},
		}

		if err := handler.ClangdConn.SendNotification("textDocument/didChange", lsp.EncodeMessage(syncEvent)); err != nil {
			logger("    error reinitilizing clangd:", err)
			return err
		}
	} else {
		// Otherwise start clangd!
		dataFolder, err := extractDataFolderFromArduinoCLI(logger)
		if err != nil {
			logger("    error: %s", err)
		}
		clangdStdout, clangdStdin, clangdStderr := startClangd(logger, handler.buildPath, handler.buildSketchCpp, dataFolder)

		clangdStdio := streams.NewReadWriteCloser(clangdStdin, clangdStdout)
		if enableLogging {
			clangdStdio = streams.LogReadWriteCloserAs(clangdStdio, "inols-clangd.log")
			go io.Copy(streams.OpenLogFileAs("inols-clangd-err.log"), clangdStderr)
		} else {
			go io.Copy(os.Stderr, clangdStderr)
		}

		rpcLogger := streams.NewJsonRPCLogger("IDE     LS", "CL", true)
		handler.ClangdConn = jsonrpc.NewConnection(clangdStdio, clangdStdio,
			func(ctx context.Context, method string, params json.RawMessage, respCallback func(result json.RawMessage, err *jsonrpc.ResponseError)) {
				logger, idx := rpcLogger.LogServerRequest(method, params)
				handler.HandleRequestFromClangd(ctx, logger, method, params, func(result json.RawMessage, err *jsonrpc.ResponseError) {
					rpcLogger.LogClientResponse(idx, method, result, err)
					respCallback(result, err)
				})
			},
			func(ctx context.Context, method string, params json.RawMessage) {
				logger := rpcLogger.LogClientNotification(method, params)
				handler.HandleNotificationFromClangd(ctx, logger, method, params)
			},
			func(e error) {
				logger("connection error with clangd! %s", e)
				handler.Close()
			},
		)
		go func() {
			handler.ClangdConn.Run()
			logger("Lost connection with clangd!")
			handler.Close()
		}()

		// Send initialization command to clangd
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if rawResp, clangErr, err := handler.ClangdConn.SendRequest(ctx, "initialize", lsp.EncodeMessage(handler.lspInitializeParams)); err != nil {
			logger("    error initilizing clangd: %v", err)
			return err
		} else if clangErr != nil {
			logger("    error initilizing clangd: %v", clangErr.AsError())
			return clangErr.AsError()
		} else if resp, err := lsp.DecodeResponseResult("initialize", rawResp); err != nil {
			logger("    error initilizing clangd: %v", err)
			return err
		} else {
			logger("    clangd successfully started: %v", resp)
		}

		if err := handler.ClangdConn.SendNotification("initialized", lsp.EncodeMessage(lsp.InitializedParams{})); err != nil {
			logger("    error sending initialized notification to clangd: %v", err)
			return err
		}
	}

	handler.buildSketchSymbolsLoad = true
	return nil
}

func extractDataFolderFromArduinoCLI(logger streams.PrefixLogger) (*paths.Path, error) {
	// XXX: do this from IDE or via gRPC
	args := []string{globalCliPath,
		"--config-file", globalCliConfigPath,
		"config",
		"dump",
		"--format", "json",
	}
	cmd, err := executils.NewProcess(args...)
	if err != nil {
		return nil, errors.Errorf("running %s: %s", strings.Join(args, " "), err)
	}
	cmdOutput := &bytes.Buffer{}
	cmd.RedirectStdoutTo(cmdOutput)
	logger("running: %s", strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		return nil, errors.Errorf("running %s: %s", strings.Join(args, " "), err)
	}

	type cmdRes struct {
		Directories struct {
			Data string `json:"data"`
		} `json:"directories"`
	}
	var res cmdRes
	if err := json.Unmarshal(cmdOutput.Bytes(), &res); err != nil {
		return nil, errors.Errorf("parsing arduino-cli output: %s", err)
	}
	// Return only the build path
	logger("Arduino Data Dir -> %s", res.Directories.Data)
	return paths.New(res.Directories.Data), nil
}

func (handler *InoHandler) refreshCppDocumentSymbols(logger streams.PrefixLogger) error {
	// Query source code symbols
	handler.readUnlock(logger)
	cppURI := lsp.NewDocumentURIFromPath(handler.buildSketchCpp)
	logger("requesting documentSymbol for %s", cppURI)
	respRaw, resErr, err := handler.ClangdConn.SendRequest(context.Background(), "textDocument/documentSymbol",
		lsp.EncodeMessage(&lsp.DocumentSymbolParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: cppURI},
		}))
	handler.readLock(logger, true)

	if err != nil {
		logger("error: %s", err)
		return fmt.Errorf("quering source code symbols: %w", err)
	}
	if resErr != nil {
		logger("error: %s", resErr.AsError())
		return fmt.Errorf("quering source code symbols: %w", resErr.AsError())
	}
	result, err := lsp.DecodeResponseResult("textDocument/documentSymbol", respRaw)
	if err != nil {
		logger("invalid response: %s", err)
		return fmt.Errorf("quering source code symbols: invalid response: %w", err)
	}

	symbols, ok := result.([]lsp.DocumentSymbol)
	if !ok {
		logger("error: expected DocumenSymbol array but got %T", result)
		return fmt.Errorf("expected DocumenSymbol but got %T", result)
	}

	// Filter non-functions symbols
	i := 0
	for _, symbol := range symbols {
		if symbol.Kind != lsp.SymbolKindFunction {
			continue
		}
		symbols[i] = symbol
		i++
	}
	symbols = symbols[:i]
	handler.buildSketchSymbols = symbols

	symbolsCanary := ""
	for _, symbol := range symbols {
		logger("   symbol: %s %s %s", symbol.Kind, symbol.Name, symbol.Range)
		if symbolText, err := textutils.ExtractRange(handler.sketchMapper.CppText.Text, symbol.Range); err != nil {
			logger("     > invalid range: %s", err)
			symbolsCanary += "/"
		} else if end := strings.Index(symbolText, "{"); end != -1 {
			logger("     TRIMMED> %s", symbolText[:end])
			symbolsCanary += symbolText[:end]
		} else {
			logger("            > %s", symbolText)
			symbolsCanary += symbolText
		}
	}
	handler.buildSketchSymbolsCanary = symbolsCanary
	return nil
}

func (handler *InoHandler) LoadCppDocumentSymbols() error {
	logger := streams.NewPrefixLogger(color.New(color.FgHiBlue), "SYLD --- ")
	defer logger("(done)")
	handler.readLock(logger, true)
	defer handler.readUnlock(logger)
	return handler.refreshCppDocumentSymbols(logger)
}

func (handler *InoHandler) CheckCppDocumentSymbols() error {
	logger := streams.NewPrefixLogger(color.New(color.FgHiBlue), "SYCK --- ")
	defer logger("(done)")
	handler.readLock(logger, true)
	defer handler.readUnlock(logger)

	oldSymbols := handler.buildSketchSymbols
	canary := handler.buildSketchSymbolsCanary
	if err := handler.refreshCppDocumentSymbols(logger); err != nil {
		return err
	}
	if len(oldSymbols) != len(handler.buildSketchSymbols) || canary != handler.buildSketchSymbolsCanary {
		logger("function symbols change detected, triggering sketch rebuild!")
		handler.scheduleRebuildEnvironment()
	}
	return nil
}

func (handler *InoHandler) CheckCppIncludesChanges() {
	logger := streams.NewPrefixLogger(color.New(color.FgHiBlue), "INCK --- ")
	logger("check for Cpp Include Changes")
	includesCanary := ""
	for _, line := range strings.Split(handler.sketchMapper.CppText.Text, "\n") {
		if strings.Contains(line, "#include ") {
			includesCanary += line
		}
	}

	if includesCanary != handler.buildSketchIncludesCanary {
		handler.buildSketchIncludesCanary = includesCanary
		logger("#include change detected, triggering sketch rebuild!")
		handler.scheduleRebuildEnvironment()
	}
}

func canonicalizeCompileCommandsJSON(compileCommandsDir *paths.Path) map[string]bool {
	// Open compile_commands.json and find the main cross-compiler executable
	compileCommandsJSONPath := compileCommandsDir.Join("compile_commands.json")
	compileCommands, err := builder.LoadCompilationDatabase(compileCommandsJSONPath)
	if err != nil {
		panic("could not find compile_commands.json")
	}
	compilers := map[string]bool{}
	for i, cmd := range compileCommands.Contents {
		if len(cmd.Arguments) == 0 {
			panic("invalid empty argument field in compile_commands.json")
		}

		// clangd requires full path to compiler (including extension .exe on Windows!)
		compilerPath := paths.New(cmd.Arguments[0]).Canonical()
		compiler := compilerPath.String()
		if runtime.GOOS == "windows" && strings.ToLower(compilerPath.Ext()) != ".exe" {
			compiler += ".exe"
		}
		compileCommands.Contents[i].Arguments[0] = compiler

		compilers[compiler] = true
	}

	// Save back compile_commands.json with OS native file separator and extension
	compileCommands.SaveToFile()

	return compilers
}

func startClangd(logger streams.PrefixLogger, compileCommandsDir, sketchCpp *paths.Path, dataFolder *paths.Path) (io.WriteCloser, io.ReadCloser, io.ReadCloser) {
	// Start clangd
	args := []string{
		globalClangdPath,
		"-log=verbose",
		fmt.Sprintf(`--compile-commands-dir=%s`, compileCommandsDir),
	}
	if dataFolder != nil {
		args = append(args, fmt.Sprintf("-query-driver=%s", dataFolder.Join("packages", "**")))
	}
	if enableLogging {
		logger("    Starting clangd: %s", strings.Join(args, " "))
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

func (handler *InoHandler) didOpen(logger streams.PrefixLogger, inoDidOpen *lsp.DidOpenTextDocumentParams) (*lsp.DidOpenTextDocumentParams, error) {
	// Add the TextDocumentItem in the tracked files list
	inoItem := inoDidOpen.TextDocument
	handler.docs[inoItem.URI.AsPath().String()] = &inoItem

	// If we are tracking a .ino...
	if inoItem.URI.Ext() == ".ino" {
		handler.sketchTrackedFilesCount++
		logger("    increasing .ino tracked files count: %d", handler.sketchTrackedFilesCount)

		// notify clang that sketchCpp has been opened only once
		if handler.sketchTrackedFilesCount != 1 {
			return nil, nil
		}
	}

	cppItem, err := handler.ino2cppTextDocumentItem(logger, inoItem)
	return &lsp.DidOpenTextDocumentParams{
		TextDocument: cppItem,
	}, err
}

func (handler *InoHandler) didClose(logger streams.PrefixLogger, inoDidClose *lsp.DidCloseTextDocumentParams) (*lsp.DidCloseTextDocumentParams, error) {
	inoIdentifier := inoDidClose.TextDocument
	if _, exist := handler.docs[inoIdentifier.URI.AsPath().String()]; exist {
		delete(handler.docs, inoIdentifier.URI.AsPath().String())
	} else {
		logger("    didClose of untracked document: %s", inoIdentifier.URI)
		return nil, unknownURI(inoIdentifier.URI)
	}

	// If we are tracking a .ino...
	if inoIdentifier.URI.Ext() == ".ino" {
		handler.sketchTrackedFilesCount--
		logger("    decreasing .ino tracked files count: %d", handler.sketchTrackedFilesCount)

		// notify clang that sketchCpp has been close only once all .ino are closed
		if handler.sketchTrackedFilesCount != 0 {
			return nil, nil
		}
	}

	cppIdentifier, err := handler.ino2cppTextDocumentIdentifier(logger, inoIdentifier)
	return &lsp.DidCloseTextDocumentParams{
		TextDocument: cppIdentifier,
	}, err
}

func (handler *InoHandler) ino2cppTextDocumentItem(logger streams.PrefixLogger, inoItem lsp.TextDocumentItem) (cppItem lsp.TextDocumentItem, err error) {
	cppURI, err := handler.ino2cppDocumentURI(logger, inoItem.URI)
	if err != nil {
		return cppItem, err
	}
	cppItem.URI = cppURI

	if cppURI.AsPath().EquivalentTo(handler.buildSketchCpp) {
		cppItem.LanguageID = "cpp"
		cppItem.Text = handler.sketchMapper.CppText.Text
		cppItem.Version = handler.sketchMapper.CppText.Version
	} else {
		cppItem.LanguageID = inoItem.LanguageID
		inoPath := inoItem.URI.AsPath().String()
		cppItem.Text = handler.docs[inoPath].Text
		cppItem.Version = handler.docs[inoPath].Version
	}

	return cppItem, nil
}

func (handler *InoHandler) didChange(logger streams.PrefixLogger, req *lsp.DidChangeTextDocumentParams) (*lsp.DidChangeTextDocumentParams, error) {
	doc := req.TextDocument

	trackedDoc, ok := handler.docs[doc.URI.AsPath().String()]
	if !ok {
		return nil, unknownURI(doc.URI)
	}
	textutils.ApplyLSPTextDocumentContentChangeEvent(trackedDoc, req.ContentChanges, doc.Version)

	// If changes are applied to a .ino file we increment the global .ino.cpp versioning
	// for each increment of the single .ino file.
	if doc.URI.Ext() == ".ino" {

		cppChanges := []lsp.TextDocumentContentChangeEvent{}
		for _, inoChange := range req.ContentChanges {
			cppRange, ok := handler.sketchMapper.InoToCppLSPRangeOk(doc.URI, inoChange.Range)
			if !ok {
				return nil, errors.Errorf("invalid change range %s:%s", doc.URI, inoChange.Range)
			}

			// Detect changes in critical lines (for example function definitions)
			// and trigger arduino-preprocessing + clangd restart.
			dirty := false
			for _, sym := range handler.buildSketchSymbols {
				if sym.SelectionRange.Overlaps(cppRange) {
					dirty = true
					logger("--! DIRTY CHANGE detected using symbol tables, force sketch rebuild!")
					break
				}
			}
			if handler.sketchMapper.ApplyTextChange(doc.URI, inoChange) {
				dirty = true
				logger("--! DIRTY CHANGE detected with sketch mapper, force sketch rebuild!")
			}
			if dirty {
				handler.scheduleRebuildEnvironment()
			}

			// logger("New version:----------")
			// logger(handler.sketchMapper.CppText.Text)
			// logger("----------------------")

			cppChange := lsp.TextDocumentContentChangeEvent{
				Range:       cppRange,
				RangeLength: inoChange.RangeLength,
				Text:        inoChange.Text,
			}
			cppChanges = append(cppChanges, cppChange)
		}

		handler.CheckCppIncludesChanges()

		// build a cpp equivalent didChange request
		cppReq := &lsp.DidChangeTextDocumentParams{
			ContentChanges: cppChanges,
			TextDocument: lsp.VersionedTextDocumentIdentifier{
				TextDocumentIdentifier: lsp.TextDocumentIdentifier{
					URI: lsp.NewDocumentURIFromPath(handler.buildSketchCpp),
				},
				Version: handler.sketchMapper.CppText.Version,
			},
		}

		return cppReq, nil
	}

	// If changes are applied to other files pass them by converting just the URI
	cppDoc, err := handler.ino2cppVersionedTextDocumentIdentifier(logger, req.TextDocument)
	if err != nil {
		return nil, err
	}
	cppReq := &lsp.DidChangeTextDocumentParams{
		TextDocument:   cppDoc,
		ContentChanges: req.ContentChanges,
	}
	return cppReq, err
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
	go handler.showMessage(ctx, lsp.MessageTypeError, message)
	return errors.New(message)
}

func (handler *InoHandler) ino2cppVersionedTextDocumentIdentifier(logger streams.PrefixLogger, doc lsp.VersionedTextDocumentIdentifier) (lsp.VersionedTextDocumentIdentifier, error) {
	cppURI, err := handler.ino2cppDocumentURI(logger, doc.URI)
	res := doc
	res.URI = cppURI
	return res, err
}

func (handler *InoHandler) ino2cppTextDocumentIdentifier(logger streams.PrefixLogger, doc lsp.TextDocumentIdentifier) (lsp.TextDocumentIdentifier, error) {
	cppURI, err := handler.ino2cppDocumentURI(logger, doc.URI)
	res := doc
	res.URI = cppURI
	return res, err
}

func (handler *InoHandler) ino2cppDocumentURI(logger streams.PrefixLogger, inoURI lsp.DocumentURI) (lsp.DocumentURI, error) {
	// Sketchbook/Sketch/Sketch.ino      -> build-path/sketch/Sketch.ino.cpp
	// Sketchbook/Sketch/AnotherTab.ino  -> build-path/sketch/Sketch.ino.cpp  (different section from above)
	// Sketchbook/Sketch/AnotherFile.cpp -> build-path/sketch/AnotherFile.cpp (1:1)
	// another/path/source.cpp           -> unchanged

	// Convert sketch path to build path
	inoPath := inoURI.AsPath()
	if inoPath.Ext() == ".ino" {
		return lsp.NewDocumentURIFromPath(handler.buildSketchCpp), nil
	}

	inside, err := inoPath.IsInsideDir(handler.sketchRoot)
	if err != nil {
		logger("    could not determine if '%s' is inside '%s'", inoPath, handler.sketchRoot)
		return lsp.NilURI, unknownURI(inoURI)
	}
	if !inside {
		logger("    '%s' not inside sketchroot '%s', passing doc identifier to as-is", handler.sketchRoot, inoPath)
		return inoURI, nil
	}

	rel, err := handler.sketchRoot.RelTo(inoPath)
	if err == nil {
		cppPath := handler.buildSketchRoot.JoinPath(rel)
		logger("    URI: '%s' -> '%s'", inoPath, cppPath)
		return lsp.NewDocumentURIFromPath(cppPath), nil
	}

	logger("    could not determine rel-path of '%s' in '%s': %s", inoPath, handler.sketchRoot, err)
	return lsp.NilURI, err
}

func (handler *InoHandler) inoDocumentURIFromInoPath(logger streams.PrefixLogger, inoPath string) (lsp.DocumentURI, error) {
	if inoPath == sourcemapper.NotIno.File {
		return sourcemapper.NotInoURI, nil
	}
	doc, ok := handler.docs[inoPath]
	if !ok {
		logger("    !!! Unresolved .ino path: %s", inoPath)
		logger("    !!! Known doc paths are:")
		for p := range handler.docs {
			logger("    !!! > %s", p)
		}
		uri := lsp.NewDocumentURI(inoPath)
		return uri, unknownURI(uri)
	}
	return doc.URI, nil
}

func (handler *InoHandler) cpp2inoDocumentURI(logger streams.PrefixLogger, cppURI lsp.DocumentURI, cppRange lsp.Range) (lsp.DocumentURI, lsp.Range, error) {
	// TODO: Split this function into 2
	//       - Cpp2inoSketchDocumentURI: converts sketch     (cppURI, cppRange) -> (inoURI, inoRange)
	//       - Cpp2inoDocumentURI      : converts non-sketch (cppURI)           -> (inoURI)              [range is the same]

	// Sketchbook/Sketch/Sketch.ino      <- build-path/sketch/Sketch.ino.cpp
	// Sketchbook/Sketch/AnotherTab.ino  <- build-path/sketch/Sketch.ino.cpp  (different section from above)
	// Sketchbook/Sketch/AnotherFile.cpp <- build-path/sketch/AnotherFile.cpp (1:1)
	// another/path/source.cpp           <- unchanged

	// Convert build path to sketch path
	cppPath := cppURI.AsPath()
	if cppPath.EquivalentTo(handler.buildSketchCpp) {
		inoPath, inoRange, err := handler.sketchMapper.CppToInoRangeOk(cppRange)
		if err == nil {
			if handler.sketchMapper.IsPreprocessedCppLine(cppRange.Start.Line) {
				inoPath = sourcemapper.NotIno.File
				logger("    URI: is in preprocessed section")
				logger("         converted %s to %s:%s", cppRange, inoPath, inoRange)
			} else {
				logger("    URI: converted %s to %s:%s", cppRange, inoPath, inoRange)
			}
		} else if _, ok := err.(sourcemapper.AdjustedRangeErr); ok {
			logger("    URI: converted %s to %s:%s (END LINE ADJUSTED)", cppRange, inoPath, inoRange)
			err = nil
		} else {
			logger("    URI: ERROR: %s", err)
			handler.sketchMapper.DebugLogAll()
			return lsp.NilURI, lsp.NilRange, err
		}
		inoURI, err := handler.inoDocumentURIFromInoPath(logger, inoPath)
		return inoURI, inoRange, err
	}

	inside, err := cppPath.IsInsideDir(handler.buildSketchRoot)
	if err != nil {
		logger("    could not determine if '%s' is inside '%s'", cppPath, handler.buildSketchRoot)
		return lsp.NilURI, lsp.NilRange, err
	}
	if !inside {
		logger("    '%s' is not inside '%s'", cppPath, handler.buildSketchRoot)
		logger("    keep doc identifier to '%s' as-is", cppPath)
		return cppURI, cppRange, nil
	}

	rel, err := handler.buildSketchRoot.RelTo(cppPath)
	if err == nil {
		inoPath := handler.sketchRoot.JoinPath(rel).String()
		logger("    URI: '%s' -> '%s'", cppPath, inoPath)
		inoURI, err := handler.inoDocumentURIFromInoPath(logger, inoPath)
		logger("              as URI: '%s'", inoURI)
		return inoURI, cppRange, err
	}

	logger("    could not determine rel-path of '%s' in '%s': %s", cppPath, handler.buildSketchRoot, err)
	return lsp.NilURI, lsp.NilRange, err
}

func (handler *InoHandler) ino2cppTextDocumentPositionParams(logger streams.PrefixLogger, inoParams lsp.TextDocumentPositionParams) (lsp.TextDocumentPositionParams, error) {
	res := lsp.TextDocumentPositionParams{}
	cppDoc, err := handler.ino2cppTextDocumentIdentifier(logger, inoParams.TextDocument)
	if err != nil {
		return res, err
	}
	cppPosition := inoParams.Position
	inoURI := inoParams.TextDocument.URI
	if inoURI.Ext() == ".ino" {
		if cppLine, ok := handler.sketchMapper.InoToCppLineOk(inoURI, inoParams.Position.Line); ok {
			cppPosition.Line = cppLine
		} else {
			logger("    invalid line requested: %s:%d", inoURI, inoParams.Position.Line)
			return res, unknownURI(inoURI)
		}
	}
	res.TextDocument = cppDoc
	res.Position = cppPosition
	return res, nil
}

func (handler *InoHandler) ino2cppRange(logger streams.PrefixLogger, inoURI lsp.DocumentURI, inoRange lsp.Range) (lsp.DocumentURI, lsp.Range, error) {
	cppURI, err := handler.ino2cppDocumentURI(logger, inoURI)
	if err != nil {
		return lsp.NilURI, lsp.Range{}, err
	}
	if cppURI.AsPath().EquivalentTo(handler.buildSketchCpp) {
		cppRange := handler.sketchMapper.InoToCppLSPRange(inoURI, inoRange)
		return cppURI, cppRange, nil
	}
	return cppURI, inoRange, nil
}

func (handler *InoHandler) ino2cppDocumentRangeFormattingParams(logger streams.PrefixLogger, inoParams *lsp.DocumentRangeFormattingParams) (*lsp.DocumentRangeFormattingParams, error) {
	cppTextDocument, err := handler.ino2cppTextDocumentIdentifier(logger, inoParams.TextDocument)
	if err != nil {
		return nil, err
	}

	_, cppRange, err := handler.ino2cppRange(logger, inoParams.TextDocument.URI, inoParams.Range)
	return &lsp.DocumentRangeFormattingParams{
		TextDocument: cppTextDocument,
		Range:        cppRange,
		Options:      inoParams.Options,
	}, err
}

func (handler *InoHandler) ino2cppDocumentOnTypeFormattingParams(params *lsp.DocumentOnTypeFormattingParams) error {
	panic("not implemented")
	// handler.sketchToBuildPathTextDocumentIdentifier(&params.TextDocument)
	// if data, ok := handler.data[params.TextDocument.URI]; ok {
	// 	params.Position.Line = data.sourceMap.InoToCppLine(data.sourceURI, params.Position.Line)
	// 	return nil
	// }
	return unknownURI(params.TextDocument.URI)
}

func (handler *InoHandler) ino2cppRenameParams(params *lsp.RenameParams) error {
	panic("not implemented")
	// handler.sketchToBuildPathTextDocumentIdentifier(&params.TextDocument)
	// if data, ok := handler.data[params.TextDocument.URI]; ok {
	// 	params.Position.Line = data.sourceMap.InoToCppLine(data.sourceURI, params.Position.Line)
	// 	return nil
	// }
	return unknownURI(params.TextDocument.URI)
}

func (handler *InoHandler) ino2cppDidChangeWatchedFilesParams(params *lsp.DidChangeWatchedFilesParams) error {
	panic("not implemented")
	// for index := range params.Changes {
	// 	fileEvent := &params.Changes[index]
	// 	if data, ok := handler.data[fileEvent.URI]; ok {
	// 		fileEvent.URI = data.targetURI
	// 	}
	// }
	return nil
}

func (handler *InoHandler) ino2cppExecuteCommand(executeCommand *lsp.ExecuteCommandParams) error {
	panic("not implemented")
	// if len(executeCommand.Arguments) == 1 {
	// 	arg := handler.parseCommandArgument(executeCommand.Arguments[0])
	// 	if workspaceEdit, ok := arg.(*lsp.WorkspaceEdit); ok {
	// 		executeCommand.Arguments[0] = handler.ino2cppWorkspaceEdit(workspaceEdit)
	// 	}
	// }
	return nil
}

func (handler *InoHandler) ino2cppWorkspaceEdit(origEdit *lsp.WorkspaceEdit) *lsp.WorkspaceEdit {
	panic("not implemented")
	newEdit := lsp.WorkspaceEdit{}
	// for uri, edit := range origEdit.Changes {
	// 	if data, ok := handler.data[lsp.DocumentURI(uri)]; ok {
	// 		newValue := make([]lsp.TextEdit, len(edit))
	// 		for index := range edit {
	// 			newValue[index] = lsp.TextEdit{
	// 				NewText: edit[index].NewText,
	// 				Range:   data.sourceMap.InoToCppLSPRange(data.sourceURI, edit[index].Range),
	// 			}
	// 		}
	// 		newEdit.Changes[string(data.targetURI)] = newValue
	// 	} else {
	// 		newEdit.Changes[uri] = edit
	// 	}
	// }
	return &newEdit
}

func (handler *InoHandler) transformClangdResult(logger streams.PrefixLogger, method string, inoURI, cppURI lsp.DocumentURI, result interface{}) interface{} {
	cppToIno := inoURI != lsp.NilURI && inoURI.AsPath().EquivalentTo(handler.buildSketchCpp)

	switch r := result.(type) {
	case *lsp.Hover:
		// method "textDocument/hover"
		if len(r.Contents.Value) == 0 {
			return nil
		}
		if cppToIno {
			_, *r.Range = handler.sketchMapper.CppToInoRange(*r.Range)
		}
		logger("<-- hover(%s)", strconv.Quote(r.Contents.Value))
		return r

	case *lsp.CompletionList:
		// method "textDocument/completion"
		newItems := make([]lsp.CompletionItem, 0)

		for _, item := range r.Items {
			if !strings.HasPrefix(item.InsertText, "_") {
				if cppToIno && item.TextEdit != nil {
					_, item.TextEdit.Range = handler.sketchMapper.CppToInoRange(item.TextEdit.Range)
				}
				newItems = append(newItems, item)
			}
		}
		r.Items = newItems
		logger("<-- completion(%d items) cppToIno=%v", len(r.Items), cppToIno)
		return r

	case []lsp.DocumentSymbol:
		// method "textDocument/documentSymbol"
		logger("    <-- documentSymbol(%d document symbols)", len(r))
		return handler.cpp2inoDocumentSymbols(logger, r, inoURI)

	case []lsp.SymbolInformation:
		// method "textDocument/documentSymbol"
		logger("    <-- documentSymbol(%d symbol information)", len(r))
		return handler.cpp2inoSymbolInformation(r)

	case []lsp.CommandOrCodeAction:
		// method "textDocument/codeAction"
		logger("    <-- codeAction(%d elements)", len(r))
		for i := range r {
			switch item := r[i].Get().(type) {
			case lsp.Command:
				logger("        > Command: %s", item.Title)
				r[i].Set(handler.Cpp2InoCommand(logger, item))
			case lsp.CodeAction:
				logger("        > CodeAction: %s", item.Title)
				r[i].Set(handler.cpp2inoCodeAction(logger, item, inoURI))
			}
		}
		logger("<-- codeAction(%d elements)", len(r))

	case *[]lsp.TextEdit:
		// Method: "textDocument/rangeFormatting"
		// Method: "textDocument/onTypeFormatting"
		// Method: "textDocument/formatting"
		logger("    <-- %s %s textEdit(%d elements)", method, cppURI, len(*r))
		for _, edit := range *r {
			logger("        > %s -> %s", edit.Range, strconv.Quote(edit.NewText))
		}
		sketchEdits, err := handler.cpp2inoTextEdits(logger, cppURI, *r)
		if err != nil {
			logger("ERROR converting textEdits: %s", err)
			return nil
		}

		inoEdits, ok := sketchEdits[inoURI]
		if !ok {
			inoEdits = []lsp.TextEdit{}
		}
		logger("<-- %s %s textEdit(%d elements)", method, inoURI, len(inoEdits))
		for _, edit := range inoEdits {
			logger("        > %s -> %s", edit.Range, strconv.Quote(edit.NewText))
		}
		return &inoEdits

	case *[]lsp.Location:
		// Method: "textDocument/definition"
		// Method: "textDocument/typeDefinition"
		// Method: "textDocument/implementation"
		// Method: "textDocument/references"
		inoLocations := []lsp.Location{}
		for _, cppLocation := range *r {
			inoLocation, err := handler.cpp2inoLocation(logger, cppLocation)
			if err != nil {
				logger("ERROR converting location %s:%s: %s", cppLocation.URI, cppLocation.Range, err)
				return nil
			}
			inoLocations = append(inoLocations, inoLocation)
		}
		return &inoLocations

	case *[]lsp.SymbolInformation:
		// Method: "workspace/symbol"

		inoSymbols := []lsp.SymbolInformation{}
		for _, cppSymbolInfo := range *r {
			cppLocation := cppSymbolInfo.Location
			inoLocation, err := handler.cpp2inoLocation(logger, cppLocation)
			if err != nil {
				logger("ERROR converting location %s:%s: %s", cppLocation.URI, cppLocation.Range, err)
				return nil
			}
			inoSymbolInfo := cppSymbolInfo
			inoSymbolInfo.Location = inoLocation
			inoSymbols = append(inoSymbols, inoSymbolInfo)
		}
		return &inoSymbols

	case *[]lsp.DocumentHighlight:
		// Method: "textDocument/documentHighlight"
		res := []lsp.DocumentHighlight{}
		for _, cppHL := range *r {
			inoHL, err := handler.cpp2inoDocumentHighlight(logger, &cppHL, cppURI)
			if err != nil {
				logger("ERROR converting location %s:%s: %s", cppURI, cppHL.Range, err)
				return nil
			}
			res = append(res, *inoHL)
		}
		return &res

	case *lsp.WorkspaceEdit: // "textDocument/rename":
		return handler.cpp2inoWorkspaceEdit(logger, r)
	}
	return result
}

func (handler *InoHandler) cpp2inoCodeAction(logger streams.PrefixLogger, codeAction lsp.CodeAction, uri lsp.DocumentURI) lsp.CodeAction {
	inoCodeAction := lsp.CodeAction{
		Title:       codeAction.Title,
		Kind:        codeAction.Kind,
		Edit:        handler.cpp2inoWorkspaceEdit(logger, codeAction.Edit),
		Diagnostics: codeAction.Diagnostics,
	}
	if codeAction.Command != nil {
		inoCommand := handler.Cpp2InoCommand(logger, *codeAction.Command)
		inoCodeAction.Command = &inoCommand
	}
	if uri.Ext() == ".ino" {
		for i, diag := range inoCodeAction.Diagnostics {
			_, inoCodeAction.Diagnostics[i].Range = handler.sketchMapper.CppToInoRange(diag.Range)
		}
	}
	return inoCodeAction
}

func (handler *InoHandler) Cpp2InoCommand(logger streams.PrefixLogger, command lsp.Command) lsp.Command {
	inoCommand := lsp.Command{
		Title:     command.Title,
		Command:   command.Command,
		Arguments: command.Arguments,
	}
	if command.Command == "clangd.applyTweak" {
		for i := range command.Arguments {
			v := struct {
				TweakID   string          `json:"tweakID"`
				File      lsp.DocumentURI `json:"file"`
				Selection lsp.Range       `json:"selection"`
			}{}
			if err := json.Unmarshal(command.Arguments[0], &v); err == nil {
				if v.TweakID == "ExtractVariable" {
					logger("            > converted clangd ExtractVariable")
					if v.File.AsPath().EquivalentTo(handler.buildSketchCpp) {
						inoFile, inoSelection := handler.sketchMapper.CppToInoRange(v.Selection)
						v.File = lsp.NewDocumentURI(inoFile)
						v.Selection = inoSelection
					}
				}
			}

			converted, err := json.Marshal(v)
			if err != nil {
				panic("Internal Error: json conversion of codeAcion command arguments")
			}
			inoCommand.Arguments[i] = converted
		}
	}
	return inoCommand
}

func (handler *InoHandler) cpp2inoWorkspaceEdit(logger streams.PrefixLogger, cppWorkspaceEdit *lsp.WorkspaceEdit) *lsp.WorkspaceEdit {
	if cppWorkspaceEdit == nil {
		return nil
	}
	inoWorkspaceEdit := &lsp.WorkspaceEdit{
		Changes: map[lsp.DocumentURI][]lsp.TextEdit{},
	}
	for editURI, edits := range cppWorkspaceEdit.Changes {
		// if the edits are not relative to sketch file...
		if !editURI.AsPath().EquivalentTo(handler.buildSketchCpp) {
			// ...pass them through...
			inoWorkspaceEdit.Changes[editURI] = edits
			continue
		}

		// ...otherwise convert edits to the sketch.ino.cpp into multilpe .ino edits
		for _, edit := range edits {
			inoURI, inoRange, err := handler.cpp2inoDocumentURI(logger, editURI, edit.Range)
			if err != nil {
				logger("    error converting edit %s:%s: %s", editURI, edit.Range, err)
				continue
			}
			//inoFile, inoRange := handler.sketchMapper.CppToInoRange(edit.Range)
			//inoURI := lsp.NewDocumentURI(inoFile)
			if _, have := inoWorkspaceEdit.Changes[inoURI]; !have {
				inoWorkspaceEdit.Changes[inoURI] = []lsp.TextEdit{}
			}
			inoWorkspaceEdit.Changes[inoURI] = append(inoWorkspaceEdit.Changes[inoURI], lsp.TextEdit{
				NewText: edit.NewText,
				Range:   inoRange,
			})
		}
	}
	logger("    done converting workspaceEdit")
	return inoWorkspaceEdit
}

func (handler *InoHandler) cpp2inoLocation(logger streams.PrefixLogger, cppLocation lsp.Location) (lsp.Location, error) {
	inoURI, inoRange, err := handler.cpp2inoDocumentURI(logger, cppLocation.URI, cppLocation.Range)
	return lsp.Location{
		URI:   inoURI,
		Range: inoRange,
	}, err
}

func (handler *InoHandler) cpp2inoDocumentHighlight(logger streams.PrefixLogger, cppHighlight *lsp.DocumentHighlight, cppURI lsp.DocumentURI) (*lsp.DocumentHighlight, error) {
	_, inoRange, err := handler.cpp2inoDocumentURI(logger, cppURI, cppHighlight.Range)
	if err != nil {
		return nil, err
	}
	return &lsp.DocumentHighlight{
		Kind:  cppHighlight.Kind,
		Range: inoRange,
	}, nil
}

func (handler *InoHandler) cpp2inoTextEdits(logger streams.PrefixLogger, cppURI lsp.DocumentURI, cppEdits []lsp.TextEdit) (map[lsp.DocumentURI][]lsp.TextEdit, error) {
	res := map[lsp.DocumentURI][]lsp.TextEdit{}
	for _, cppEdit := range cppEdits {
		inoURI, inoEdit, err := handler.cpp2inoTextEdit(logger, cppURI, cppEdit)
		if err != nil {
			return nil, err
		}
		inoEdits, ok := res[inoURI]
		if !ok {
			inoEdits = []lsp.TextEdit{}
		}
		inoEdits = append(inoEdits, inoEdit)
		res[inoURI] = inoEdits
	}
	return res, nil
}

func (handler *InoHandler) cpp2inoTextEdit(logger streams.PrefixLogger, cppURI lsp.DocumentURI, cppEdit lsp.TextEdit) (lsp.DocumentURI, lsp.TextEdit, error) {
	inoURI, inoRange, err := handler.cpp2inoDocumentURI(logger, cppURI, cppEdit.Range)
	inoEdit := cppEdit
	inoEdit.Range = inoRange
	return inoURI, inoEdit, err
}

func (handler *InoHandler) cpp2inoDocumentSymbols(logger streams.PrefixLogger, cppSymbols []lsp.DocumentSymbol, inoRequestedURI lsp.DocumentURI) []lsp.DocumentSymbol {
	inoRequested := inoRequestedURI.AsPath().String()
	logger("    filtering for requested ino file: %s", inoRequested)
	if inoRequestedURI.Ext() != ".ino" || len(cppSymbols) == 0 {
		return cppSymbols
	}

	inoSymbols := []lsp.DocumentSymbol{}
	for _, symbol := range cppSymbols {
		logger("    > convert %s %s", symbol.Kind, symbol.Range)
		if handler.sketchMapper.IsPreprocessedCppLine(symbol.Range.Start.Line) {
			logger("      symbol is in the preprocessed section of the sketch.ino.cpp")
			continue
		}

		inoFile, inoRange := handler.sketchMapper.CppToInoRange(symbol.Range)
		inoSelectionURI, inoSelectionRange := handler.sketchMapper.CppToInoRange(symbol.SelectionRange)

		if inoFile != inoSelectionURI {
			logger("      ERROR: symbol range and selection belongs to different URI!")
			logger("        symbol %s != selection %s", symbol.Range, symbol.SelectionRange)
			logger("        %s:%s != %s:%s", inoFile, inoRange, inoSelectionURI, inoSelectionRange)
			continue
		}

		if inoFile != inoRequested {
			logger("    skipping symbol related to %s", inoFile)
			continue
		}

		inoSymbols = append(inoSymbols, lsp.DocumentSymbol{
			Name:           symbol.Name,
			Detail:         symbol.Detail,
			Deprecated:     symbol.Deprecated,
			Kind:           symbol.Kind,
			Range:          inoRange,
			SelectionRange: inoSelectionRange,
			Children:       handler.cpp2inoDocumentSymbols(logger, symbol.Children, inoRequestedURI),
		})
	}

	return inoSymbols
}

func (handler *InoHandler) cpp2inoSymbolInformation(syms []lsp.SymbolInformation) []lsp.SymbolInformation {
	panic("not implemented")
	// // Much like in cpp2inoDocumentSymbols we de-duplicate symbols based on file in-file location.
	// idx := make(map[string]*lsp.SymbolInformation)
	// for _, sym := range syms {
	// 	handler.cpp2inoLocation(&sym.Location)

	// 	nme := fmt.Sprintf("%s::%s", sym.ContainerName, sym.Name)
	// 	other, duplicate := idx[nme]
	// 	if duplicate && other.Location.Range.Start.Line < sym.Location.Range.Start.Line {
	// 		continue
	// 	}

	// 	idx[nme] = sym
	// }

	// var j int
	// symbols := make([]lsp.SymbolInformation, len(idx))
	// for _, sym := range idx {
	// 	symbols[j] = *sym
	// 	j++
	// }
	// return symbols
}

func (handler *InoHandler) cpp2inoDiagnostics(logger streams.PrefixLogger, cppDiags *lsp.PublishDiagnosticsParams) ([]*lsp.PublishDiagnosticsParams, error) {
	inoDiagsParam := map[lsp.DocumentURI]*lsp.PublishDiagnosticsParams{}

	cppURI := cppDiags.URI
	isSketch := cppURI.AsPath().EquivalentTo(handler.buildSketchCpp)
	if isSketch {
		for inoURI := range handler.inoDocsWithDiagnostics {
			inoDiagsParam[inoURI] = &lsp.PublishDiagnosticsParams{
				URI:         inoURI,
				Diagnostics: []lsp.Diagnostic{},
			}
		}
		handler.inoDocsWithDiagnostics = map[lsp.DocumentURI]bool{}
	} else {
		inoURI, _, err := handler.cpp2inoDocumentURI(logger, cppURI, lsp.NilRange)
		if err != nil {
			return nil, err
		}
		inoDiagsParam[inoURI] = &lsp.PublishDiagnosticsParams{
			URI:         inoURI,
			Diagnostics: []lsp.Diagnostic{},
		}
	}

	for _, cppDiag := range cppDiags.Diagnostics {
		inoURI, inoRange, err := handler.cpp2inoDocumentURI(logger, cppURI, cppDiag.Range)
		if err != nil {
			return nil, err
		}
		if inoURI.String() == sourcemapper.NotInoURI.String() {
			continue
		}

		inoDiagParam, created := inoDiagsParam[inoURI]
		if !created {
			inoDiagParam = &lsp.PublishDiagnosticsParams{
				URI:         inoURI,
				Diagnostics: []lsp.Diagnostic{},
			}
			inoDiagsParam[inoURI] = inoDiagParam
		}

		inoDiag := cppDiag
		inoDiag.Range = inoRange
		inoDiagParam.Diagnostics = append(inoDiagParam.Diagnostics, inoDiag)

		if isSketch {
			handler.inoDocsWithDiagnostics[inoURI] = true

			// If we have an "undefined reference" in the .ino code trigger a
			// check for newly created symbols (that in turn may trigger a
			// new arduino-preprocessing of the sketch).
			var inoDiagCode string
			if err := json.Unmarshal(inoDiag.Code, &inoDiagCode); err != nil {
				if inoDiagCode == "undeclared_var_use_suggest" ||
					inoDiagCode == "undeclared_var_use" ||
					inoDiagCode == "ovl_no_viable_function_in_call" ||
					inoDiagCode == "pp_file_not_found" {
					handler.buildSketchSymbolsCheck = true
				}
			}
		}
	}

	inoDiagParams := []*lsp.PublishDiagnosticsParams{}
	for _, v := range inoDiagsParam {
		inoDiagParams = append(inoDiagParams, v)
	}
	return inoDiagParams, nil
}

// HandleRequestFromClangd handles a notification message received from clangd.
func (handler *InoHandler) HandleNotificationFromClangd(ctx context.Context, logger streams.PrefixLogger, method string, paramsRaw json.RawMessage) {
	defer streams.CatchAndLogPanic()

	// n := atomic.AddInt64(&handler.clangdMessageCount, 1)
	// prefix := fmt.Sprintf("CLG <-- %s notif%d ", method, n)

	params, err := lsp.DecodeNotificationParams(method, paramsRaw)
	if err != nil {
		logger("error parsing clang message:", err)
		return
	}
	if params == nil {
		// passthrough
		logger("passing through message")
		if err := handler.IDEConn.SendNotification(method, paramsRaw); err != nil {
			logger("Error sending notification to IDE: " + err.Error())
		}
		return
	}

	// method: "$/progress"
	if progress, ok := params.(lsp.ProgressParams); ok {
		var token string
		if err := json.Unmarshal(progress.Token, &token); err != nil {
			logger("error decoding progess token: %s", err)
			return
		}
		switch value := progress.TryToDecodeWellKnownValues().(type) {
		case lsp.WorkDoneProgressBegin:
			// logger("report %s %v", id, value)
			handler.progressHandler.Begin(token, &value)
		case lsp.WorkDoneProgressReport:
			// logger("report %s %v", id, value)
			handler.progressHandler.Report(token, &value)
		case lsp.WorkDoneProgressEnd:
			// logger("end %s %v", id, value)
			handler.progressHandler.End(token, &value)
		default:
			logger("error unsupported $/progress: " + string(progress.Value))
		}
		return
	}

	// Default to read lock
	handler.readLock(logger, false)
	defer handler.readUnlock(logger)

	switch p := params.(type) {
	case *lsp.PublishDiagnosticsParams:
		// "textDocument/publishDiagnostics"
		logger("publishDiagnostics(%s):", p.URI)
		for _, diag := range p.Diagnostics {
			logger("> %d:%d - %v: %s", diag.Range.Start.Line, diag.Range.Start.Character, diag.Severity, diag.Code)
		}

		// the diagnostics on sketch.cpp.ino once mapped into their
		// .ino counter parts may span over multiple .ino files...
		inoDiagnostics, err := handler.cpp2inoDiagnostics(logger, p)
		if err != nil {
			logger("    Error converting diagnostics to .ino: %s", err)
			return
		}

		// Push back to IDE the converted diagnostics
		for _, inoDiag := range inoDiagnostics {
			logger("to IDE: publishDiagnostics(%s):", inoDiag.URI)
			for _, diag := range inoDiag.Diagnostics {
				logger("> %d:%d - %v: %s", diag.Range.Start.Line, diag.Range.Start.Character, diag.Severity, diag.Code)
			}
			if err := handler.IDEConn.SendNotification("textDocument/publishDiagnostics", lsp.EncodeMessage(inoDiag)); err != nil {
				logger("    Error sending diagnostics to IDE: %s", err)
				return
			}
		}
		return
	}
	if err != nil {
		logger("From clangd: Method:", method, "Error:", err)
		return
	}

	logger("to IDE")
	if err := handler.IDEConn.SendNotification(method, lsp.EncodeMessage(params)); err != nil {
		logger("Error sending notification to IDE: " + err.Error())
	}
}

// HandleRequestFromClangd handles a request message received from clangd.
func (handler *InoHandler) HandleRequestFromClangd(ctx context.Context, logger streams.PrefixLogger,
	method string, paramsRaw json.RawMessage,
	respCallback func(result json.RawMessage, err *jsonrpc.ResponseError),
) {
	defer streams.CatchAndLogPanic()

	// n := atomic.AddInt64(&handler.clangdMessageCount, 1)
	// prefix := fmt.Sprintf("CLG <-- %s %v ", method, n)

	params, err := lsp.DecodeRequestParams(method, paramsRaw)
	if err != nil {
		logger("Error parsing clang message: %v", err)
		respCallback(nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()})
		return
	}

	if method == "window/workDoneProgress/create" {
		// server initiated progress
		var createReq lsp.WorkDoneProgressCreateParams
		if err := json.Unmarshal(paramsRaw, &createReq); err != nil {
			logger("error decoding window/workDoneProgress/create: %v", err)
			respCallback(nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()})
			return
		}

		var token string
		if err := json.Unmarshal(createReq.Token, &token); err != nil {
			logger("error decoding progess token: %s", err)
			respCallback(nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()})
			return
		}
		handler.progressHandler.Create(token)
		respCallback(lsp.EncodeMessage(struct{}{}), nil)
		return
	}

	// Default to read lock
	handler.readLock(logger, false)
	defer handler.readUnlock(logger)

	switch p := params.(type) {
	case *lsp.ApplyWorkspaceEditParams:
		// "workspace/applyEdit"
		p.Edit = *handler.cpp2inoWorkspaceEdit(logger, &p.Edit)
	}
	if err != nil {
		logger("From clangd: Method: %s, Error: %v", method, err)
		respCallback(nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()})
		return
	}

	respRaw := lsp.EncodeMessage(params)
	if params == nil {
		// passthrough
		logger("passing through message")
		respRaw = paramsRaw
	}

	logger("to IDE")
	resp, respErr, err := handler.IDEConn.SendRequest(ctx, method, respRaw)
	if err != nil {
		logger("Error sending request to IDE:", err)
		respCallback(nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()})
		return
	}

	respCallback(resp, respErr)
}

func (handler *InoHandler) createClangdFormatterConfig(logger streams.PrefixLogger, cppuri lsp.DocumentURI) (func(), error) {
	// clangd looks for a .clang-format configuration file on the same directory
	// pointed by the uri passed in the lsp command parameters.
	// https://github.com/llvm/llvm-project/blob/64d06ed9c9e0389cd27545d2f6e20455a91d89b1/clang-tools-extra/clangd/ClangdLSPServer.cpp#L856-L868
	// https://github.com/llvm/llvm-project/blob/64d06ed9c9e0389cd27545d2f6e20455a91d89b1/clang-tools-extra/clangd/ClangdServer.cpp#L402-L404

	config := `# See: https://releases.llvm.org/11.0.1/tools/clang/docs/ClangFormatStyleOptions.html
---
Language: Cpp
# LLVM is the default style setting, used when a configuration option is not set here
BasedOnStyle: LLVM
AccessModifierOffset: -2
AlignAfterOpenBracket: Align
AlignConsecutiveAssignments: false
AlignConsecutiveBitFields: false
AlignConsecutiveDeclarations: false
AlignConsecutiveMacros: false
AlignEscapedNewlines: DontAlign
AlignOperands: Align
AlignTrailingComments: true
AllowAllArgumentsOnNextLine: true
AllowAllConstructorInitializersOnNextLine: true
AllowAllParametersOfDeclarationOnNextLine: true
AllowShortBlocksOnASingleLine: Always
AllowShortCaseLabelsOnASingleLine: true
AllowShortEnumsOnASingleLine: true
AllowShortFunctionsOnASingleLine: Empty
AllowShortIfStatementsOnASingleLine: Always
AllowShortLambdasOnASingleLine: Empty
AllowShortLoopsOnASingleLine: true
AlwaysBreakAfterDefinitionReturnType: None
AlwaysBreakAfterReturnType: None
AlwaysBreakBeforeMultilineStrings: false
AlwaysBreakTemplateDeclarations: No
BinPackArguments: true
BinPackParameters: true
# Only used when "BreakBeforeBraces" set to "Custom"
BraceWrapping:
  AfterCaseLabel: false
  AfterClass: false
  AfterControlStatement: Never
  AfterEnum: false
  AfterFunction: false
  AfterNamespace: false
  #AfterObjCDeclaration:
  AfterStruct: false
  AfterUnion: false
  AfterExternBlock: false
  BeforeCatch: false
  BeforeElse: false
  BeforeLambdaBody: false
  BeforeWhile: false
  IndentBraces: false
  SplitEmptyFunction: false
  SplitEmptyRecord: false
  SplitEmptyNamespace: false
# Java-specific
#BreakAfterJavaFieldAnnotations:
BreakBeforeBinaryOperators: NonAssignment
BreakBeforeBraces: Attach
BreakBeforeTernaryOperators: true
BreakConstructorInitializers: BeforeColon
BreakInheritanceList: BeforeColon
BreakStringLiterals: false
ColumnLimit: 0
# "" matches none
CommentPragmas: ""
CompactNamespaces: false
ConstructorInitializerAllOnOneLineOrOnePerLine: true
ConstructorInitializerIndentWidth: 2
ContinuationIndentWidth: 2
Cpp11BracedListStyle: false
DeriveLineEnding: true
DerivePointerAlignment: true
DisableFormat: false
# Docs say "Do not use this in config files". The default (LLVM 11.0.1) is "false".
#ExperimentalAutoDetectBinPacking:
FixNamespaceComments: false
ForEachMacros: []
IncludeBlocks: Preserve
IncludeCategories: []
# "" matches none
IncludeIsMainRegex: ""
IncludeIsMainSourceRegex: ""
IndentCaseBlocks: true
IndentCaseLabels: true
IndentExternBlock: Indent
IndentGotoLabels: false
IndentPPDirectives: None
IndentWidth: 2
IndentWrappedFunctionNames: false
InsertTrailingCommas: None
# Java-specific
#JavaImportGroups:
# JavaScript-specific
#JavaScriptQuotes:
#JavaScriptWrapImports
KeepEmptyLinesAtTheStartOfBlocks: true
MacroBlockBegin: ""
MacroBlockEnd: ""
# Set to a large number to effectively disable
MaxEmptyLinesToKeep: 100000
NamespaceIndentation: None
NamespaceMacros: []
# Objective C-specific
#ObjCBinPackProtocolList:
#ObjCBlockIndentWidth:
#ObjCBreakBeforeNestedBlockParam:
#ObjCSpaceAfterProperty:
#ObjCSpaceBeforeProtocolList
PenaltyBreakAssignment: 1
PenaltyBreakBeforeFirstCallParameter: 1
PenaltyBreakComment: 1
PenaltyBreakFirstLessLess: 1
PenaltyBreakString: 1
PenaltyBreakTemplateDeclaration: 1
PenaltyExcessCharacter: 1
PenaltyReturnTypeOnItsOwnLine: 1
# Used as a fallback if alignment style can't be detected from code (DerivePointerAlignment: true)
PointerAlignment: Right
RawStringFormats: []
ReflowComments: false
SortIncludes: false
SortUsingDeclarations: false
SpaceAfterCStyleCast: false
SpaceAfterLogicalNot: false
SpaceAfterTemplateKeyword: false
SpaceBeforeAssignmentOperators: true
SpaceBeforeCpp11BracedList: false
SpaceBeforeCtorInitializerColon: true
SpaceBeforeInheritanceColon: true
SpaceBeforeParens: ControlStatements
SpaceBeforeRangeBasedForLoopColon: true
SpaceBeforeSquareBrackets: false
SpaceInEmptyBlock: false
SpaceInEmptyParentheses: false
SpacesBeforeTrailingComments: 2
SpacesInAngles: false
SpacesInCStyleCastParentheses: false
SpacesInConditionalStatement: false
SpacesInContainerLiterals: false
SpacesInParentheses: false
SpacesInSquareBrackets: false
Standard: Auto
StatementMacros: []
TabWidth: 2
TypenameMacros: []
# Default to LF if line endings can't be detected from the content (DeriveLineEnding).
UseCRLF: false
UseTab: Never
WhitespaceSensitiveMacros: []
`

	try := func(conf *paths.Path) bool {
		if c, err := conf.ReadFile(); err != nil {
			logger("    error reading custom formatter config file %s: %s", conf, err)
		} else {
			logger("    using custom formatter config file %s", conf)
			config = string(c)
		}
		return true
	}

	if sketchFormatterConf := handler.sketchRoot.Join(".clang-format"); sketchFormatterConf.Exist() {
		// If a custom config is present in the sketch folder, use that one
		try(sketchFormatterConf)
	} else if globalFormatterConf != nil && globalFormatterConf.Exist() {
		// Otherwise if a global config file is present, use that one
		try(globalFormatterConf)
	}

	targetFile := cppuri.AsPath()
	if targetFile.IsNotDir() {
		targetFile = targetFile.Parent()
	}
	targetFile = targetFile.Join(".clang-format")
	cleanup := func() {
		targetFile.Remove()
		logger("    formatter config cleaned")
	}
	logger("    writing formatter config in: %s", targetFile)
	err := targetFile.WriteFile([]byte(config))
	return cleanup, err
}

func (handler *InoHandler) showMessage(ctx context.Context, msgType lsp.MessageType, message string) {
	defer streams.CatchAndLogPanic()

	params := lsp.ShowMessageParams{
		Type:    msgType,
		Message: message,
	}
	if err := handler.IDEConn.SendNotification("window/showMessage", lsp.EncodeMessage(params)); err != nil {
		// TODO: Log?
	}
}

func unknownURI(uri lsp.DocumentURI) error {
	return errors.New("Document is not available: " + uri.String())
}
