package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
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

// INOLanguageServer is a JSON-RPC handler that delegates messages to clangd.
type INOLanguageServer struct {
	IDEConn *lsp.Server
	Clangd  *ClangdClient

	progressHandler *ProgressProxyHandler

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
	buildSketchSymbolsLoad     chan bool
	buildSketchSymbolsCheck    chan bool
	rebuildSketchDeadline      *time.Time
	rebuildSketchDeadlineMutex sync.Mutex
	sketchRoot                 *paths.Path
	sketchName                 string
	sketchMapper               *sourcemapper.InoMapper
	sketchTrackedFilesCount    int
	docs                       map[string]lsp.TextDocumentItem
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

func (handler *INOLanguageServer) writeLock(logger jsonrpc.FunctionLogger, requireClangd bool) {
	handler.dataMux.Lock()
	logger.Logf(yellow.Sprintf("write-locked"))
	if requireClangd {
		handler.waitClangdStart(logger)
	}
}

func (handler *INOLanguageServer) writeUnlock(logger jsonrpc.FunctionLogger) {
	logger.Logf(yellow.Sprintf("write-unlocked"))
	handler.dataMux.Unlock()
}

func (handler *INOLanguageServer) readLock(logger jsonrpc.FunctionLogger, requireClangd bool) {
	handler.dataMux.RLock()
	logger.Logf(yellow.Sprintf("read-locked"))

	for requireClangd && handler.Clangd == nil {
		// if clangd is not started...

		// Release the read lock and acquire a write lock
		// (this is required to wait on condition variable and restart clang).
		logger.Logf(yellow.Sprintf("clang not started: read-unlocking..."))
		handler.dataMux.RUnlock()

		handler.writeLock(logger, true)
		handler.writeUnlock(logger)

		handler.dataMux.RLock()
		logger.Logf(yellow.Sprintf("testing again if clang started: read-locked..."))
	}
}

func (handler *INOLanguageServer) readUnlock(logger jsonrpc.FunctionLogger) {
	logger.Logf(yellow.Sprintf("read-unlocked"))
	handler.dataMux.RUnlock()
}

func (handler *INOLanguageServer) waitClangdStart(logger jsonrpc.FunctionLogger) error {
	if handler.Clangd != nil {
		return nil
	}

	logger.Logf("(throttled: waiting for clangd)")
	logger.Logf(yellow.Sprintf("unlocked (waiting clangd)"))
	handler.clangdStarted.Wait()
	logger.Logf(yellow.Sprintf("locked (waiting clangd)"))

	if handler.Clangd == nil {
		logger.Logf("clangd startup failed: aborting call")
		return errors.New("could not start clangd, aborted")
	}
	return nil
}

// NewINOLanguageServer creates and configures an Arduino Language Server.
func NewINOLanguageServer(stdin io.Reader, stdout io.Writer, board Board) *INOLanguageServer {
	logger := NewLSPFunctionLogger(color.HiWhiteString, "LS: ")
	handler := &INOLanguageServer{
		docs:                    map[string]lsp.TextDocumentItem{},
		inoDocsWithDiagnostics:  map[lsp.DocumentURI]bool{},
		closing:                 make(chan bool),
		buildSketchSymbolsLoad:  make(chan bool, 1),
		buildSketchSymbolsCheck: make(chan bool, 1),
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
		logger.Logf("Initial board configuration: %s", board)
		logger.Logf("Language server build path: %s", handler.buildPath)
		logger.Logf("Language server build sketch root: %s", handler.buildSketchRoot)
	}

	handler.IDEConn = lsp.NewServer(stdin, stdout, handler)
	handler.IDEConn.SetLogger(&LSPLogger{IncomingPrefix: "IDE --> LS", OutgoingPrefix: "IDE <-- LS"})
	handler.progressHandler = NewProgressProxy(handler.IDEConn)

	go func() {
		defer streams.CatchAndLogPanic()
		handler.IDEConn.Run()
		logger.Logf("Lost connection with IDE!")
		handler.Close()
	}()

	go func() {
		defer streams.CatchAndLogPanic()
		for {
			select {
			case <-handler.buildSketchSymbolsLoad:
				// ...also un-queue buildSketchSymbolsCheck
				select {
				case <-handler.buildSketchSymbolsCheck:
				default:
				}
				handler.LoadCppDocumentSymbols()

			case <-handler.buildSketchSymbolsCheck:
				handler.CheckCppDocumentSymbols()

			case <-handler.closing:
				return
			}
		}
	}()

	go func() {
		defer streams.CatchAndLogPanic()
		handler.rebuildEnvironmentLoop()
	}()
	return handler
}

func (handler *INOLanguageServer) queueLoadCppDocumentSymbols() {
	select {
	case handler.buildSketchSymbolsLoad <- true:
	default:
	}
}

func (handler *INOLanguageServer) queueCheckCppDocumentSymbols() {
	select {
	case handler.buildSketchSymbolsCheck <- true:
	default:
	}
}

func (handler *INOLanguageServer) LoadCppDocumentSymbols() error {
	logger := NewLSPFunctionLogger(color.HiBlueString, "SYLD --- ")
	defer logger.Logf("(done)")
	handler.readLock(logger, true)
	defer handler.readUnlock(logger)

	return handler.refreshCppDocumentSymbols(logger)
}

func (handler *INOLanguageServer) CheckCppDocumentSymbols() error {
	logger := NewLSPFunctionLogger(color.HiBlueString, "SYCK --- ")
	defer logger.Logf("(done)")
	handler.readLock(logger, true)
	defer handler.readUnlock(logger)

	oldSymbols := handler.buildSketchSymbols
	canary := handler.buildSketchSymbolsCanary
	if err := handler.refreshCppDocumentSymbols(logger); err != nil {
		return err
	}
	if len(oldSymbols) != len(handler.buildSketchSymbols) || canary != handler.buildSketchSymbolsCanary {
		logger.Logf("function symbols change detected, triggering sketch rebuild!")
		handler.scheduleRebuildEnvironment()
	}
	return nil
}

func (handler *INOLanguageServer) startClangd(inoParams *lsp.InitializeParams) {
	logger := NewLSPFunctionLogger(color.HiCyanString, "INIT --- ")
	logger.Logf("initializing workbench")

	// Start clangd asynchronously
	handler.writeLock(logger, false) // do not wait for clangd... we are starting it :-)
	defer handler.writeUnlock(logger)

	// TODO: Inline this function
	handler.initializeWorkbench(logger, inoParams)

	// signal that clangd is running now...
	handler.clangdStarted.Broadcast()

	logger.Logf("Done initializing workbench")
}

func (handler *INOLanguageServer) Initialize(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.InitializeParams) (*lsp.InitializeResult, *jsonrpc.ResponseError) {
	go func() {
		defer streams.CatchAndLogPanic()
		handler.startClangd(inoParams)
	}()

	resp := &lsp.InitializeResult{
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
	}
	logger.Logf("initialization parameters: %s", string(lsp.EncodeMessage(resp)))
	return resp, nil
}

func (handler *INOLanguageServer) Shutdown(context.Context, jsonrpc.FunctionLogger) *jsonrpc.ResponseError {
	panic("unimplemented")
}

func (handler *INOLanguageServer) WorkspaceSymbol(context.Context, jsonrpc.FunctionLogger, *lsp.WorkspaceSymbolParams) ([]lsp.SymbolInformation, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) WorkspaceExecuteCommand(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.ExecuteCommandParams) (json.RawMessage, *jsonrpc.ResponseError) {
	handler.readLock(logger, true)
	defer handler.readUnlock(logger)
	// return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesMethodNotFound, Message: "Unimplemented"}
	// err = handler.ino2cppExecuteCommand(inoParams)
	panic("unimplemented")
}

func (handler *INOLanguageServer) WorkspaceWillCreateFiles(context.Context, jsonrpc.FunctionLogger, *lsp.CreateFilesParams) (*lsp.WorkspaceEdit, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) WorkspaceWillRenameFiles(context.Context, jsonrpc.FunctionLogger, *lsp.RenameFilesParams) (*lsp.WorkspaceEdit, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) WorkspaceWillDeleteFiles(context.Context, jsonrpc.FunctionLogger, *lsp.DeleteFilesParams) (*lsp.WorkspaceEdit, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) TextDocumentWillSaveWaitUntil(context.Context, jsonrpc.FunctionLogger, *lsp.WillSaveTextDocumentParams) ([]lsp.TextEdit, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) TextDocumentCompletion(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.CompletionParams) (*lsp.CompletionList, *jsonrpc.ResponseError) {
	handler.readLock(logger, true)
	defer handler.readUnlock(logger)

	logger.Logf("--> completion(%s)\n", inoParams.TextDocument)
	cppTextDocPositionParams, err := handler.ino2cppTextDocumentPositionParams(logger, inoParams.TextDocumentPositionParams)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	cppParams := inoParams
	cppParams.TextDocumentPositionParams = cppTextDocPositionParams
	logger.Logf("    --> completion(%s)\n", inoParams.TextDocument)
	inoURI := inoParams.TextDocument.URI

	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	clangResp, clangErr, err := handler.Clangd.conn.TextDocumentCompletion(ctx, cppParams)
	if err != nil {
		logger.Logf("clangd connection error: %v", err)
		handler.Close()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response error: %v", clangErr.AsError())
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	cppToIno := inoURI != lsp.NilURI && inoURI.AsPath().EquivalentTo(handler.buildSketchCpp)

	inoResp := *clangResp
	inoItems := make([]lsp.CompletionItem, 0)
	for _, item := range clangResp.Items {
		if !strings.HasPrefix(item.InsertText, "_") {
			if cppToIno && item.TextEdit != nil {
				_, item.TextEdit.Range = handler.sketchMapper.CppToInoRange(item.TextEdit.Range)
			}
			inoItems = append(inoItems, item)
		}
	}
	inoResp.Items = inoItems
	logger.Logf("<-- completion(%d items) cppToIno=%v", len(inoResp.Items), cppToIno)
	return &inoResp, nil
}

func (handler *INOLanguageServer) CompletionItemResolve(context.Context, jsonrpc.FunctionLogger, *lsp.CompletionItem) (*lsp.CompletionItem, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) TextDocumentHover(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.HoverParams) (*lsp.Hover, *jsonrpc.ResponseError) {
	handler.readLock(logger, true)
	defer handler.readUnlock(logger)

	inoURI := inoParams.TextDocument.URI
	inoTextDocPosition := inoParams.TextDocumentPositionParams
	logger.Logf("--> hover(%s)\n", inoTextDocPosition)

	cppTextDocPosition, err := handler.ino2cppTextDocumentPositionParams(logger, inoTextDocPosition)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	logger.Logf("    --> hover(%s)\n", cppTextDocPosition)
	cppParams := &lsp.HoverParams{
		TextDocumentPositionParams: cppTextDocPosition,
	}
	clangResp, clangErr, err := handler.Clangd.conn.TextDocumentHover(ctx, cppParams)
	if err != nil {
		logger.Logf("clangd connectiono error: %v", err)
		handler.Close()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response error: %v", clangErr.AsError())
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	if clangResp == nil {
		return nil, nil
	}

	inoResp := *clangResp
	// TODO: ????
	// if len(clangResp.Contents.Value) == 0 {
	// 	return nil
	// }
	cppToIno := inoURI != lsp.NilURI && inoURI.AsPath().EquivalentTo(handler.buildSketchCpp)
	if cppToIno {
		_, inoRange := handler.sketchMapper.CppToInoRange(*clangResp.Range)
		inoResp.Range = &inoRange
	}
	logger.Logf("<-- hover(%s)", strconv.Quote(inoResp.Contents.Value))
	return &inoResp, nil
}

func (handler *INOLanguageServer) TextDocumentSignatureHelp(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.SignatureHelpParams) (*lsp.SignatureHelp, *jsonrpc.ResponseError) {
	handler.readLock(logger, true)
	defer handler.readUnlock(logger)

	inoTextDocumentPosition := inoParams.TextDocumentPositionParams

	logger.Logf("%s", inoTextDocumentPosition)
	cppTextDocumentPosition, err := handler.ino2cppTextDocumentPositionParams(logger, inoTextDocumentPosition)
	if err == nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	logger.Logf("-> %s", cppTextDocumentPosition)
	cppParams := *inoParams
	cppParams.TextDocumentPositionParams = cppTextDocumentPosition
	cppSignatureHelp, cppErr, err := handler.Clangd.conn.TextDocumentSignatureHelp(ctx, inoParams)
	if err != nil {
		logger.Logf("clangd connectiono error: %v", err)
		handler.Close()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if cppErr != nil {
		logger.Logf("clangd response error: %v", cppErr.AsError())
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: cppErr.AsError().Error()}
	}

	// No need to convert back to inoSignatureHelp

	return cppSignatureHelp, nil
}

func (handler *INOLanguageServer) TextDocumentDeclaration(context.Context, jsonrpc.FunctionLogger, *lsp.DeclarationParams) ([]lsp.Location, []lsp.LocationLink, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) TextDocumentDefinition(ctx context.Context, logger jsonrpc.FunctionLogger, p *lsp.DefinitionParams) ([]lsp.Location, []lsp.LocationLink, *jsonrpc.ResponseError) {
	handler.readLock(logger, true)
	defer handler.readUnlock(logger)

	inoTextDocPosition := p.TextDocumentPositionParams

	logger.Logf("%s", inoTextDocPosition)
	cppTextDocPosition, err := handler.ino2cppTextDocumentPositionParams(logger, inoTextDocPosition)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	logger.Logf("-> %s", cppTextDocPosition)
	cppParams := *p
	cppParams.TextDocumentPositionParams = cppTextDocPosition
	cppLocations, cppLocationLinks, cppErr, err := handler.Clangd.conn.TextDocumentDefinition(ctx, &cppParams)
	if err != nil {
		logger.Logf("clangd connectiono error: %v", err)
		handler.Close()
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if cppErr != nil {
		logger.Logf("clangd response error: %v", cppErr.AsError())
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: cppErr.AsError().Error()}
	}

	var inoLocations []lsp.Location
	if cppLocations != nil {
		inoLocations, err = handler.cpp2inoLocationArray(logger, cppLocations)
		if err != nil {
			handler.Close()
			return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
		}
	}

	var inoLocationLinks []lsp.LocationLink
	if cppLocationLinks != nil {
		panic("unimplemented")
	}

	return inoLocations, inoLocationLinks, nil
}

func (handler *INOLanguageServer) cpp2inoLocationArray(logger jsonrpc.FunctionLogger, cppLocations []lsp.Location) ([]lsp.Location, error) {
	inoLocations := []lsp.Location{}
	for _, cppLocation := range cppLocations {
		inoLocation, err := handler.cpp2inoLocation(logger, cppLocation)
		if err != nil {
			logger.Logf("ERROR converting location %s: %s", cppLocation, err)
			return nil, err
		}
		inoLocations = append(inoLocations, inoLocation)
	}
	return inoLocations, nil
}

func (handler *INOLanguageServer) TextDocumentTypeDefinition(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.TypeDefinitionParams) ([]lsp.Location, []lsp.LocationLink, *jsonrpc.ResponseError) {
	handler.readLock(logger, true)
	defer handler.readUnlock(logger)

	inoTextDocumentPosition := inoParams.TextDocumentPositionParams

	logger.Logf("%s", inoTextDocumentPosition)
	// inoURI := inoTextDocumentPosition.TextDocument.URI
	cppTextDocumentPosition, err := handler.ino2cppTextDocumentPositionParams(logger, inoTextDocumentPosition)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	// cppURI := cppTextDocumentPosition.TextDocument.URI
	logger.Logf("-> %s", cppTextDocumentPosition)

	cppParams := *inoParams
	cppParams.TextDocumentPositionParams = cppTextDocumentPosition
	cppLocations, cppLocationLinks, cppErr, err := handler.Clangd.conn.TextDocumentTypeDefinition(ctx, &cppParams)
	if err != nil {
		logger.Logf("clangd connectiono error: %v", err)
		handler.Close()
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if cppErr != nil {
		logger.Logf("clangd response error: %v", cppErr.AsError())
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: cppErr.AsError().Error()}
	}

	var inoLocations []lsp.Location
	if cppLocations != nil {
		inoLocations, err = handler.cpp2inoLocationArray(logger, cppLocations)
		if err != nil {
			handler.Close()
			return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
		}
	}

	var inoLocationLinks []lsp.LocationLink
	if cppLocationLinks != nil {
		panic("unimplemented")
	}

	return inoLocations, inoLocationLinks, nil
}

func (handler *INOLanguageServer) TextDocumentImplementation(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.ImplementationParams) ([]lsp.Location, []lsp.LocationLink, *jsonrpc.ResponseError) {
	handler.readLock(logger, true)
	defer handler.readUnlock(logger)

	inoTextDocumentPosition := inoParams.TextDocumentPositionParams
	logger.Logf("%s", inoTextDocumentPosition)

	cppTextDocumentPosition, err := handler.ino2cppTextDocumentPositionParams(logger, inoTextDocumentPosition)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	logger.Logf("-> %s", cppTextDocumentPosition)
	cppParams := *inoParams
	cppParams.TextDocumentPositionParams = cppTextDocumentPosition
	cppLocations, cppLocationLinks, cppErr, err := handler.Clangd.conn.TextDocumentImplementation(ctx, &cppParams)
	if err != nil {
		logger.Logf("clangd connectiono error: %v", err)
		handler.Close()
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if cppErr != nil {
		logger.Logf("clangd response error: %v", cppErr.AsError())
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: cppErr.AsError().Error()}
	}

	var inoLocations []lsp.Location
	if cppLocations != nil {
		inoLocations, err = handler.cpp2inoLocationArray(logger, cppLocations)
		if err != nil {
			handler.Close()
			return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
		}
	}

	var inoLocationLinks []lsp.LocationLink
	if cppLocationLinks != nil {
		panic("unimplemented")
	}

	return inoLocations, inoLocationLinks, nil
}

func (handler *INOLanguageServer) TextDocumentReferences(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.ReferenceParams) ([]lsp.Location, *jsonrpc.ResponseError) {
	handler.readLock(logger, true)
	defer handler.readUnlock(logger)
	panic("unimplemented")
	// inoURI = p.TextDocument.URI
	// _, err = handler.ino2cppTextDocumentPositionParams(logger, p.TextDocumentPositionParams)
}

func (handler *INOLanguageServer) TextDocumentDocumentHighlight(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.DocumentHighlightParams) ([]lsp.DocumentHighlight, *jsonrpc.ResponseError) {
	handler.readLock(logger, true)
	defer handler.readUnlock(logger)

	inoTextDocumentPosition := inoParams.TextDocumentPositionParams
	cppTextDocumentPosition, err := handler.ino2cppTextDocumentPositionParams(logger, inoTextDocumentPosition)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	cppURI := cppTextDocumentPosition.TextDocument.URI

	cppParams := *inoParams
	cppParams.TextDocumentPositionParams = cppTextDocumentPosition
	cppHighlights, clangErr, err := handler.Clangd.conn.TextDocumentDocumentHighlight(ctx, &cppParams)
	if err != nil {
		logger.Logf("clangd connectiono error: %v", err)
		handler.Close()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response error: %v", clangErr.AsError())
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	if cppHighlights != nil {
		return nil, nil
	}

	inoHighlights := []lsp.DocumentHighlight{}
	for _, cppHighlight := range cppHighlights {
		inoHighlight, err := handler.cpp2inoDocumentHighlight(logger, cppHighlight, cppURI)
		if err != nil {
			logger.Logf("ERROR converting location %s:%s: %s", cppURI, cppHighlight.Range, err)
			return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
		}
		inoHighlights = append(inoHighlights, inoHighlight)
	}
	return inoHighlights, nil
}

func (handler *INOLanguageServer) TextDocumentDocumentSymbol(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.DocumentSymbolParams) ([]lsp.DocumentSymbol, []lsp.SymbolInformation, *jsonrpc.ResponseError) {
	handler.readLock(logger, true)
	defer handler.readUnlock(logger)

	inoTextDocument := inoParams.TextDocument
	inoURI := inoTextDocument.URI
	logger.Logf("--> %s")

	cppTextDocument, err := handler.ino2cppTextDocumentIdentifier(logger, inoTextDocument)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	cppParams := *inoParams
	cppParams.TextDocument = cppTextDocument
	logger.Logf("    --> documentSymbol(%s)", cppTextDocument)
	cppDocSymbols, cppSymbolInformation, clangErr, err := handler.Clangd.conn.TextDocumentDocumentSymbol(ctx, &cppParams)
	if err != nil {
		logger.Logf("clangd connectiono error: %v", err)
		handler.Close()
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response error: %v", clangErr.AsError())
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	var inoDocSymbols []lsp.DocumentSymbol
	if cppDocSymbols != nil {
		logger.Logf("    <-- documentSymbol(%d document symbols)", len(cppDocSymbols))
		inoDocSymbols = handler.cpp2inoDocumentSymbols(logger, cppDocSymbols, inoURI)
	}
	var inoSymbolInformation []lsp.SymbolInformation
	if cppSymbolInformation != nil {
		logger.Logf("    <-- documentSymbol(%d symbol information)", len(cppSymbolInformation))
		inoSymbolInformation = handler.cpp2inoSymbolInformation(cppSymbolInformation)
	}
	return inoDocSymbols, inoSymbolInformation, nil
}

func (handler *INOLanguageServer) TextDocumentCodeAction(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.CodeActionParams) ([]lsp.CommandOrCodeAction, *jsonrpc.ResponseError) {
	handler.readLock(logger, true)
	defer handler.readUnlock(logger)

	inoTextDocument := inoParams.TextDocument
	inoURI := inoTextDocument.URI
	logger.Logf("--> codeAction(%s:%s)", inoTextDocument, inoParams.Range.Start)

	cppParams := *inoParams
	cppTextDocument, err := handler.ino2cppTextDocumentIdentifier(logger, inoTextDocument)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	cppParams.TextDocument = cppTextDocument

	if cppTextDocument.URI.AsPath().EquivalentTo(handler.buildSketchCpp) {
		cppParams.Range = handler.sketchMapper.InoToCppLSPRange(inoURI, inoParams.Range)
		for i, inoDiag := range inoParams.Context.Diagnostics {
			cppParams.Context.Diagnostics[i].Range = handler.sketchMapper.InoToCppLSPRange(inoURI, inoDiag.Range)
		}
	}
	logger.Logf("    --> codeAction(%s:%s)", cppParams.TextDocument, inoParams.Range.Start)

	cppResp, cppErr, err := handler.Clangd.conn.TextDocumentCodeAction(ctx, &cppParams)
	if err != nil {
		logger.Logf("clangd connectiono error: %v", err)
		handler.Close()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if cppErr != nil {
		logger.Logf("clangd response error: %v", cppErr.AsError())
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: cppErr.AsError().Error()}
	}

	// TODO: Create a function for this one?
	inoResp := []lsp.CommandOrCodeAction{}
	if cppResp != nil {
		logger.Logf("    <-- codeAction(%d elements)", len(cppResp))
		for _, cppItem := range cppResp {
			inoItem := lsp.CommandOrCodeAction{}
			switch i := cppItem.Get().(type) {
			case lsp.Command:
				logger.Logf("        > Command: %s", i.Title)
				inoItem.Set(handler.cpp2inoCommand(logger, i))
			case lsp.CodeAction:
				logger.Logf("        > CodeAction: %s", i.Title)
				inoItem.Set(handler.cpp2inoCodeAction(logger, i, inoURI))
			}
			inoResp = append(inoResp, inoItem)
		}
		logger.Logf("<-- codeAction(%d elements)", len(inoResp))
	}
	return inoResp, nil
}

func (handler *INOLanguageServer) CodeActionResolve(context.Context, jsonrpc.FunctionLogger, *lsp.CodeAction) (*lsp.CodeAction, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) TextDocumentCodeLens(context.Context, jsonrpc.FunctionLogger, *lsp.CodeLensParams) ([]lsp.CodeLens, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) CodeLensResolve(context.Context, jsonrpc.FunctionLogger, *lsp.CodeLens) (*lsp.CodeLens, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) TextDocumentDocumentLink(context.Context, jsonrpc.FunctionLogger, *lsp.DocumentLinkParams) ([]lsp.DocumentLink, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) DocumentLinkResolve(context.Context, jsonrpc.FunctionLogger, *lsp.DocumentLink) (*lsp.DocumentLink, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) TextDocumentDocumentColor(context.Context, jsonrpc.FunctionLogger, *lsp.DocumentColorParams) ([]lsp.ColorInformation, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) TextDocumentColorPresentation(context.Context, jsonrpc.FunctionLogger, *lsp.ColorPresentationParams) ([]lsp.ColorPresentation, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) TextDocumentFormatting(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.DocumentFormattingParams) ([]lsp.TextEdit, *jsonrpc.ResponseError) {
	handler.readLock(logger, true)
	defer handler.readUnlock(logger)

	inoTextDocument := inoParams.TextDocument
	inoURI := inoTextDocument.URI
	logger.Logf("--> formatting(%s)", inoTextDocument)

	cppTextDocument, err := handler.ino2cppTextDocumentIdentifier(logger, inoTextDocument)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	cppURI := cppTextDocument.URI

	logger.Logf("    --> formatting(%s)", cppTextDocument)

	if cleanup, e := handler.createClangdFormatterConfig(logger, cppURI); e != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	} else {
		defer cleanup()
	}

	cppParams := *inoParams
	cppParams.TextDocument = cppTextDocument
	cppEdits, clangErr, err := handler.Clangd.conn.TextDocumentFormatting(ctx, &cppParams)
	if err != nil {
		logger.Logf("clangd connectiono error: %v", err)
		handler.Close()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response error: %v", clangErr.AsError())
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	if cppEdits == nil {
		return nil, nil
	}

	sketchEdits, err := handler.cpp2inoTextEdits(logger, cppURI, cppEdits)
	if err != nil {
		logger.Logf("ERROR converting textEdits: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if inoEdits, ok := sketchEdits[inoURI]; !ok {
		return []lsp.TextEdit{}, nil
	} else {
		return inoEdits, nil
	}
}

func (handler *INOLanguageServer) TextDocumentRangeFormatting(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.DocumentRangeFormattingParams) ([]lsp.TextEdit, *jsonrpc.ResponseError) {
	handler.readLock(logger, true)
	defer handler.readUnlock(logger)

	// Method: "textDocument/rangeFormatting"
	logger.Logf("%s", inoParams.TextDocument)
	inoURI := inoParams.TextDocument.URI
	cppParams, err := handler.ino2cppDocumentRangeFormattingParams(logger, inoParams)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	cppURI := cppParams.TextDocument.URI
	logger.Logf("-> %s", cppParams.TextDocument)
	if cleanup, e := handler.createClangdFormatterConfig(logger, cppURI); e != nil {
		logger.Logf("cannot create formatter config file: %v", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	} else {
		defer cleanup()
	}

	cppEdits, clangErr, err := handler.Clangd.conn.TextDocumentRangeFormatting(ctx, cppParams)
	if err != nil {
		logger.Logf("clangd connectiono error: %v", err)
		handler.Close()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response error: %v", clangErr.AsError())
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	// Transform and return the result
	if cppEdits != nil {
		return nil, nil
	}

	sketchEdits, err := handler.cpp2inoTextEdits(logger, cppURI, cppEdits)
	if err != nil {
		logger.Logf("ERROR converting textEdits: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if inoEdits, ok := sketchEdits[inoURI]; !ok {
		return []lsp.TextEdit{}, nil
	} else {
		return inoEdits, nil
	}
}

func (handler *INOLanguageServer) TextDocumentOnTypeFormatting(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.DocumentOnTypeFormattingParams) ([]lsp.TextEdit, *jsonrpc.ResponseError) {
	handler.readLock(logger, true)
	defer handler.readUnlock(logger)
	// return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesMethodNotFound, Message: "Unimplemented"}
	// inoURI = p.TextDocument.URI
	// err = handler.ino2cppDocumentOnTypeFormattingParams(p)
	panic("unimplemented")
}

func (handler *INOLanguageServer) TextDocumentRename(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.RenameParams) (*lsp.WorkspaceEdit, *jsonrpc.ResponseError) {
	handler.readLock(logger, true)
	defer handler.readUnlock(logger)
	// return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesMethodNotFound, Message: "Unimplemented"}
	// inoURI = p.TextDocument.URI
	// err = handler.ino2cppRenameParams(p)
	panic("unimplemented")
}

func (handler *INOLanguageServer) TextDocumentFoldingRange(context.Context, jsonrpc.FunctionLogger, *lsp.FoldingRangeParams) ([]lsp.FoldingRange, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) TextDocumentSelectionRange(context.Context, jsonrpc.FunctionLogger, *lsp.SelectionRangeParams) ([]lsp.SelectionRange, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) TextDocumentPrepareCallHierarchy(context.Context, jsonrpc.FunctionLogger, *lsp.CallHierarchyPrepareParams) ([]lsp.CallHierarchyItem, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) CallHierarchyIncomingCalls(context.Context, jsonrpc.FunctionLogger, *lsp.CallHierarchyIncomingCallsParams) ([]lsp.CallHierarchyIncomingCall, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) CallHierarchyOutgoingCalls(context.Context, jsonrpc.FunctionLogger, *lsp.CallHierarchyOutgoingCallsParams) ([]lsp.CallHierarchyOutgoingCall, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) TextDocumentSemanticTokensFull(context.Context, jsonrpc.FunctionLogger, *lsp.SemanticTokensParams) (*lsp.SemanticTokens, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) TextDocumentSemanticTokensFullDelta(context.Context, jsonrpc.FunctionLogger, *lsp.SemanticTokensDeltaParams) (*lsp.SemanticTokens, *lsp.SemanticTokensDelta, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) TextDocumentSemanticTokensRange(context.Context, jsonrpc.FunctionLogger, *lsp.SemanticTokensRangeParams) (*lsp.SemanticTokens, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) WorkspaceSemanticTokensRefresh(context.Context, jsonrpc.FunctionLogger) *jsonrpc.ResponseError {
	panic("unimplemented")
}

func (handler *INOLanguageServer) TextDocumentLinkedEditingRange(context.Context, jsonrpc.FunctionLogger, *lsp.LinkedEditingRangeParams) (*lsp.LinkedEditingRanges, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) TextDocumentMoniker(context.Context, jsonrpc.FunctionLogger, *lsp.MonikerParams) ([]lsp.Moniker, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// Notifications from IDE ->

func (handler *INOLanguageServer) Progress(logger jsonrpc.FunctionLogger, progress *lsp.ProgressParams) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) Initialized(logger jsonrpc.FunctionLogger, params *lsp.InitializedParams) {
	logger.Logf("Notification is not propagated to clangd")
}

func (handler *INOLanguageServer) Exit(jsonrpc.FunctionLogger) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) SetTrace(jsonrpc.FunctionLogger, *lsp.SetTraceParams) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) WindowWorkDoneProgressCancel(jsonrpc.FunctionLogger, *lsp.WorkDoneProgressCancelParams) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) WorkspaceDidChangeWorkspaceFolders(jsonrpc.FunctionLogger, *lsp.DidChangeWorkspaceFoldersParams) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) WorkspaceDidChangeConfiguration(jsonrpc.FunctionLogger, *lsp.DidChangeConfigurationParams) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) WorkspaceDidChangeWatchedFiles(logger jsonrpc.FunctionLogger, inoParams *lsp.DidChangeWatchedFilesParams) {
	handler.readLock(logger, true)
	defer handler.readUnlock(logger)
	// return
	// err = handler.ino2cppDidChangeWatchedFilesParams(p)
	panic("unimplemented")
}

func (handler *INOLanguageServer) WorkspaceDidCreateFiles(jsonrpc.FunctionLogger, *lsp.CreateFilesParams) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) WorkspaceDidRenameFiles(jsonrpc.FunctionLogger, *lsp.RenameFilesParams) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) WorkspaceDidDeleteFiles(jsonrpc.FunctionLogger, *lsp.DeleteFilesParams) {
	panic("unimplemented")
}

// Notifications from Clangd <-

func (handler *INOLanguageServer) PublishDiagnosticsFromClangd(logger jsonrpc.FunctionLogger, cppParams *lsp.PublishDiagnosticsParams) {
	// Default to read lock
	handler.readLock(logger, false)
	defer handler.readUnlock(logger)

	logger.Logf("publishDiagnostics(%s):", cppParams.URI)
	for _, diag := range cppParams.Diagnostics {
		logger.Logf("> %d:%d - %v: %s", diag.Range.Start.Line, diag.Range.Start.Character, diag.Severity, diag.Code)
	}

	// the diagnostics on sketch.cpp.ino once mapped into their
	// .ino counter parts may span over multiple .ino files...
	allInoParams, err := handler.cpp2inoDiagnostics(logger, cppParams)
	if err != nil {
		logger.Logf("    Error converting diagnostics to .ino: %s", err)
		return
	}

	// Push back to IDE the converted diagnostics
	for _, inoParams := range allInoParams {
		logger.Logf("to IDE: publishDiagnostics(%s):", inoParams.URI)
		for _, diag := range inoParams.Diagnostics {
			logger.Logf("> %d:%d - %v: %s", diag.Range.Start.Line, diag.Range.Start.Character, diag.Severity, diag.Code)
		}
		logger.Logf("IDE <-- LS NOTIF textDocument/publishDiagnostics:")

		if err := handler.IDEConn.TextDocumentPublishDiagnostics(inoParams); err != nil {
			logger.Logf("    Error sending diagnostics to IDE: %s", err)
			return
		}
	}
}

func (handler *INOLanguageServer) ProgressFromClangd(logger jsonrpc.FunctionLogger, progress *lsp.ProgressParams) {
	var token string
	if err := json.Unmarshal(progress.Token, &token); err != nil {
		logger.Logf("error decoding progess token: %s", err)
		return
	}
	switch value := progress.TryToDecodeWellKnownValues().(type) {
	case lsp.WorkDoneProgressBegin:
		logger.Logf("begin %s %v", token, value)
		handler.progressHandler.Begin(token, &value)
	case lsp.WorkDoneProgressReport:
		logger.Logf("report %s %v", token, value)
		handler.progressHandler.Report(token, &value)
	case lsp.WorkDoneProgressEnd:
		logger.Logf("end %s %v", token, value)
		handler.progressHandler.End(token, &value)
	default:
		logger.Logf("error unsupported $/progress: " + string(progress.Value))
	}
}

// Requests from IDE <->

func (handler *INOLanguageServer) TextDocumentDidOpen(logger jsonrpc.FunctionLogger, inoParam *lsp.DidOpenTextDocumentParams) {
	handler.writeLock(logger, true)
	defer handler.writeUnlock(logger)

	// Add the TextDocumentItem in the tracked files list
	inoTextDocItem := inoParam.TextDocument
	handler.docs[inoTextDocItem.URI.AsPath().String()] = inoTextDocItem

	// If we are tracking a .ino...
	if inoTextDocItem.URI.Ext() == ".ino" {
		handler.sketchTrackedFilesCount++
		logger.Logf("Increasing .ino tracked files count to %d", handler.sketchTrackedFilesCount)

		// Notify clangd that sketchCpp has been opened only once
		if handler.sketchTrackedFilesCount != 1 {
			logger.Logf("Clang already notified, do not notify it anymore")
			return
		}
	}

	if cppItem, err := handler.ino2cppTextDocumentItem(logger, inoTextDocItem); err != nil {
		logger.Logf("Error: %s", err)
	} else if err := handler.Clangd.conn.TextDocumentDidOpen(&lsp.DidOpenTextDocumentParams{
		TextDocument: cppItem,
	}); err != nil {
		// Exit the process and trigger a restart by the client in case of a severe error
		logger.Logf("Error sending notification to clangd server: %v", err)
		logger.Logf("Please restart the language server.")
		handler.Close()
	}
}

func (handler *INOLanguageServer) TextDocumentDidChange(logger jsonrpc.FunctionLogger, inoParams *lsp.DidChangeTextDocumentParams) {
	handler.writeLock(logger, true)
	defer handler.writeUnlock(logger)

	logger.Logf("didChange(%s)", inoParams.TextDocument)
	for _, change := range inoParams.ContentChanges {
		logger.Logf("> %s", change)
	}

	if cppParams, err := handler.didChange(logger, inoParams); err != nil {
		logger.Logf("--E Error: %s", err)
	} else if cppParams == nil {
		logger.Logf("--X Notification is not propagated to clangd")
	} else {
		logger.Logf("LS --> CL NOTIF didChange(%s@%d)", cppParams.TextDocument)
		for _, change := range cppParams.ContentChanges {
			logger.Logf("                > %s", change)
		}
		if err := handler.Clangd.conn.TextDocumentDidChange(cppParams); err != nil {
			// Exit the process and trigger a restart by the client in case of a severe error
			logger.Logf("Connection error with clangd server: %v", err)
			logger.Logf("Please restart the language server.")
			handler.Close()
		}
	}
}

func (handler *INOLanguageServer) TextDocumentWillSave(jsonrpc.FunctionLogger, *lsp.WillSaveTextDocumentParams) {
	panic("unimplemented")
}

func (handler *INOLanguageServer) TextDocumentDidSave(logger jsonrpc.FunctionLogger, inoParams *lsp.DidSaveTextDocumentParams) {
	handler.writeLock(logger, true)
	defer handler.writeUnlock(logger)

	logger.Logf("didSave(%s)", inoParams.TextDocument)
	if cppTextDocument, err := handler.ino2cppTextDocumentIdentifier(logger, inoParams.TextDocument); err != nil {
		logger.Logf("--E Error: %s", err)
	} else if cppTextDocument.URI.AsPath().EquivalentTo(handler.buildSketchCpp) {
		logger.Logf("    didSave(%s) equals %s", cppTextDocument, handler.buildSketchCpp)
		logger.Logf("--| didSave not forwarded to clangd")
	} else {
		logger.Logf("LS --> CL NOTIF didSave(%s)", cppTextDocument)
		if err := handler.Clangd.conn.TextDocumentDidSave(&lsp.DidSaveTextDocumentParams{
			TextDocument: cppTextDocument,
			Text:         inoParams.Text,
		}); err != nil {
			// Exit the process and trigger a restart by the client in case of a severe error
			logger.Logf("Connection error with clangd server: %v", err)
			logger.Logf("Please restart the language server.")
			handler.Close()
		}
	}
}

func (handler *INOLanguageServer) TextDocumentDidClose(logger jsonrpc.FunctionLogger, inoParams *lsp.DidCloseTextDocumentParams) {
	handler.writeLock(logger, true)
	defer handler.writeUnlock(logger)

	logger.Logf("didClose(%s)", inoParams.TextDocument)

	if cppParams, err := handler.didClose(logger, inoParams); err != nil {
		logger.Logf("--E Error: %s", err)
	} else if cppParams == nil {
		logger.Logf("--X Notification is not propagated to clangd")
	} else {
		logger.Logf("--> CL NOTIF didClose(%s)", cppParams.TextDocument)
		if err := handler.Clangd.conn.TextDocumentDidClose(cppParams); err != nil {
			// Exit the process and trigger a restart by the client in case of a severe error
			logger.Logf("Error sending notification to clangd server: %v", err)
			logger.Logf("Please restart the language server.")
			handler.Close()
		}
	}
}

// Requests from Clangd <->

func (handler *INOLanguageServer) WindowWorkDoneProgressCreateFromClangd(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.WorkDoneProgressCreateParams) *jsonrpc.ResponseError {
	var token string
	if err := json.Unmarshal(params.Token, &token); err != nil {
		logger.Logf("error decoding progress token: %s", err)
		return &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	handler.progressHandler.Create(token)
	return nil
}

// Close closes all the json-rpc connections.
func (handler *INOLanguageServer) Close() {
	if handler.Clangd != nil {
		handler.Clangd.Close()
		handler.Clangd = nil
	}
	if handler.closing != nil {
		close(handler.closing)
		handler.closing = nil
	}
}

// CloseNotify returns a channel that is closed when the InoHandler is closed
func (handler *INOLanguageServer) CloseNotify() <-chan bool {
	return handler.closing
}

// CleanUp performs cleanup of the workspace and temp files create by the language server
func (handler *INOLanguageServer) CleanUp() {
	if handler.buildPath != nil {
		handler.buildPath.RemoveAll()
		handler.buildPath = nil
	}
}

func (handler *INOLanguageServer) initializeWorkbench(logger jsonrpc.FunctionLogger, params *lsp.InitializeParams) error {
	// TODO: This function must be split into two
	// -> start clang (when params != nil)
	// -> reser clang status (when params == nil)
	// the two flows shares very little

	currCppTextVersion := 0
	if params != nil {
		logger.Logf("    --> initialize(%s)", params.RootURI)
		handler.lspInitializeParams = params
		handler.sketchRoot = params.RootURI.AsPath()
		handler.sketchName = handler.sketchRoot.Base()
	} else {
		logger.Logf("    --> RE-initialize()")
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

		logger.Logf("LS --> CL NOTIF textDocument/didChange:")
		if err := handler.Clangd.conn.TextDocumentDidChange(syncEvent); err != nil {
			logger.Logf("    error reinitilizing clangd:", err)
			return err
		}
	} else {
		// Otherwise start clangd!
		dataFolder, err := extractDataFolderFromArduinoCLI(logger)
		if err != nil {
			logger.Logf("    error: %s", err)
		}

		handler.Clangd = NewClangdClient(
			logger, handler.buildPath, handler.buildSketchCpp, dataFolder,
			func() {
				logger.Logf("Lost connection with clangd!")
				handler.Close()
			}, handler)

		// Send initialization command to clangd
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		initRes, clangErr, err := handler.Clangd.conn.Initialize(ctx, handler.lspInitializeParams)
		if err != nil {
			logger.Logf("    error initilizing clangd: %v", err)
			return err
		}
		if clangErr != nil {
			logger.Logf("    error initilizing clangd: %v", clangErr.AsError())
			return clangErr.AsError()
		} else {
			logger.Logf("clangd successfully started: %s", string(lsp.EncodeMessage(initRes)))
		}

		if err := handler.Clangd.conn.Initialized(&lsp.InitializedParams{}); err != nil {
			logger.Logf("    error sending initialized notification to clangd: %v", err)
			return err
		}
	}

	handler.queueLoadCppDocumentSymbols()
	return nil
}

func extractDataFolderFromArduinoCLI(logger jsonrpc.FunctionLogger) (*paths.Path, error) {
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
	logger.Logf("running: %s", strings.Join(args, " "))
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
	logger.Logf("Arduino Data Dir -> %s", res.Directories.Data)
	return paths.New(res.Directories.Data), nil
}

func (handler *INOLanguageServer) refreshCppDocumentSymbols(logger jsonrpc.FunctionLogger) error {
	// Query source code symbols
	handler.readUnlock(logger)
	cppURI := lsp.NewDocumentURIFromPath(handler.buildSketchCpp)
	logger.Logf("requesting documentSymbol for %s", cppURI)

	cppParams := &lsp.DocumentSymbolParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: cppURI},
	}
	cppDocumentSymbols, _ /* cppSymbolInformation */, cppErr, err := handler.Clangd.conn.TextDocumentDocumentSymbol(context.Background(), cppParams)
	handler.readLock(logger, true)
	if err != nil {
		logger.Logf("error: %s", err)
		return fmt.Errorf("quering source code symbols: %w", err)
	}
	if cppErr != nil {
		logger.Logf("error: %s", cppErr.AsError())
		return fmt.Errorf("quering source code symbols: %w", cppErr.AsError())
	}

	if cppDocumentSymbols == nil {
		err := errors.New("expected DocumenSymbol array but got SymbolInformation instead")
		logger.Logf("error: %s", err)
		return err
	}

	// Filter non-functions symbols
	i := 0
	for _, symbol := range cppDocumentSymbols {
		if symbol.Kind != lsp.SymbolKindFunction {
			continue
		}
		cppDocumentSymbols[i] = symbol
		i++
	}
	cppDocumentSymbols = cppDocumentSymbols[:i]
	handler.buildSketchSymbols = cppDocumentSymbols

	symbolsCanary := ""
	for _, symbol := range cppDocumentSymbols {
		logger.Logf("   symbol: %s %s %s", symbol.Kind, symbol.Name, symbol.Range)
		if symbolText, err := textutils.ExtractRange(handler.sketchMapper.CppText.Text, symbol.Range); err != nil {
			logger.Logf("     > invalid range: %s", err)
			symbolsCanary += "/"
		} else if end := strings.Index(symbolText, "{"); end != -1 {
			logger.Logf("     TRIMMED> %s", symbolText[:end])
			symbolsCanary += symbolText[:end]
		} else {
			logger.Logf("            > %s", symbolText)
			symbolsCanary += symbolText
		}
	}
	handler.buildSketchSymbolsCanary = symbolsCanary
	return nil
}

func (handler *INOLanguageServer) CheckCppIncludesChanges() {
	logger := NewLSPFunctionLogger(color.HiBlueString, "INCK --- ")
	logger.Logf("check for Cpp Include Changes")
	includesCanary := ""
	for _, line := range strings.Split(handler.sketchMapper.CppText.Text, "\n") {
		if strings.Contains(line, "#include ") {
			includesCanary += line
		}
	}

	if includesCanary != handler.buildSketchIncludesCanary {
		handler.buildSketchIncludesCanary = includesCanary
		logger.Logf("#include change detected, triggering sketch rebuild!")
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

func (handler *INOLanguageServer) didClose(logger jsonrpc.FunctionLogger, inoDidClose *lsp.DidCloseTextDocumentParams) (*lsp.DidCloseTextDocumentParams, error) {
	inoIdentifier := inoDidClose.TextDocument
	if _, exist := handler.docs[inoIdentifier.URI.AsPath().String()]; exist {
		delete(handler.docs, inoIdentifier.URI.AsPath().String())
	} else {
		logger.Logf("    didClose of untracked document: %s", inoIdentifier.URI)
		return nil, unknownURI(inoIdentifier.URI)
	}

	// If we are tracking a .ino...
	if inoIdentifier.URI.Ext() == ".ino" {
		handler.sketchTrackedFilesCount--
		logger.Logf("    decreasing .ino tracked files count: %d", handler.sketchTrackedFilesCount)

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

func (handler *INOLanguageServer) ino2cppTextDocumentItem(logger jsonrpc.FunctionLogger, inoItem lsp.TextDocumentItem) (cppItem lsp.TextDocumentItem, err error) {
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

func (handler *INOLanguageServer) didChange(logger jsonrpc.FunctionLogger, req *lsp.DidChangeTextDocumentParams) (*lsp.DidChangeTextDocumentParams, error) {
	doc := req.TextDocument

	trackedDoc, ok := handler.docs[doc.URI.AsPath().String()]
	if !ok {
		return nil, unknownURI(doc.URI)
	}
	textutils.ApplyLSPTextDocumentContentChangeEvent(&trackedDoc, req.ContentChanges, doc.Version)

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
					logger.Logf("--! DIRTY CHANGE detected using symbol tables, force sketch rebuild!")
					break
				}
			}
			if handler.sketchMapper.ApplyTextChange(doc.URI, inoChange) {
				dirty = true
				logger.Logf("--! DIRTY CHANGE detected with sketch mapper, force sketch rebuild!")
			}
			if dirty {
				handler.scheduleRebuildEnvironment()
			}

			// logger.Logf("New version:----------")
			// logger.Logf(handler.sketchMapper.CppText.Text)
			// logger.Logf("----------------------")

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

func (handler *INOLanguageServer) handleError(logger jsonrpc.FunctionLogger, err error) error {
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
		exp, regexpErr := regexp.Compile(`([\w\.\-]+): No such file or directory`)
		if regexpErr != nil {
			panic(regexpErr)
		}
		submatch := exp.FindStringSubmatch(errorStr)
		message = "Editor support may be inaccurate because the header `" + submatch[1] + "` was not found."
		message += " If it is part of a library, use the Library Manager to install it."
	} else {
		message = "Could not start editor support.\n" + errorStr
	}
	go func() {
		defer streams.CatchAndLogPanic()
		handler.showMessage(logger, lsp.MessageTypeError, message)
	}()
	return errors.New(message)
}

func (handler *INOLanguageServer) ino2cppVersionedTextDocumentIdentifier(logger jsonrpc.FunctionLogger, doc lsp.VersionedTextDocumentIdentifier) (lsp.VersionedTextDocumentIdentifier, error) {
	cppURI, err := handler.ino2cppDocumentURI(logger, doc.URI)
	res := doc
	res.URI = cppURI
	return res, err
}

func (handler *INOLanguageServer) ino2cppTextDocumentIdentifier(logger jsonrpc.FunctionLogger, doc lsp.TextDocumentIdentifier) (lsp.TextDocumentIdentifier, error) {
	cppURI, err := handler.ino2cppDocumentURI(logger, doc.URI)
	res := doc
	res.URI = cppURI
	return res, err
}

func (handler *INOLanguageServer) ino2cppDocumentURI(logger jsonrpc.FunctionLogger, inoURI lsp.DocumentURI) (lsp.DocumentURI, error) {
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
		logger.Logf("    could not determine if '%s' is inside '%s'", inoPath, handler.sketchRoot)
		return lsp.NilURI, unknownURI(inoURI)
	}
	if !inside {
		logger.Logf("    '%s' not inside sketchroot '%s', passing doc identifier to as-is", handler.sketchRoot, inoPath)
		return inoURI, nil
	}

	rel, err := handler.sketchRoot.RelTo(inoPath)
	if err == nil {
		cppPath := handler.buildSketchRoot.JoinPath(rel)
		logger.Logf("    URI: '%s' -> '%s'", inoPath, cppPath)
		return lsp.NewDocumentURIFromPath(cppPath), nil
	}

	logger.Logf("    could not determine rel-path of '%s' in '%s': %s", inoPath, handler.sketchRoot, err)
	return lsp.NilURI, err
}

func (handler *INOLanguageServer) inoDocumentURIFromInoPath(logger jsonrpc.FunctionLogger, inoPath string) (lsp.DocumentURI, error) {
	if inoPath == sourcemapper.NotIno.File {
		return sourcemapper.NotInoURI, nil
	}
	doc, ok := handler.docs[inoPath]
	if !ok {
		logger.Logf("    !!! Unresolved .ino path: %s", inoPath)
		logger.Logf("    !!! Known doc paths are:")
		for p := range handler.docs {
			logger.Logf("    !!! > %s", p)
		}
		uri := lsp.NewDocumentURI(inoPath)
		return uri, unknownURI(uri)
	}
	return doc.URI, nil
}

func (handler *INOLanguageServer) cpp2inoDocumentURI(logger jsonrpc.FunctionLogger, cppURI lsp.DocumentURI, cppRange lsp.Range) (lsp.DocumentURI, lsp.Range, error) {
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
				logger.Logf("    URI: is in preprocessed section")
				logger.Logf("         converted %s to %s:%s", cppRange, inoPath, inoRange)
			} else {
				logger.Logf("    URI: converted %s to %s:%s", cppRange, inoPath, inoRange)
			}
		} else if _, ok := err.(sourcemapper.AdjustedRangeErr); ok {
			logger.Logf("    URI: converted %s to %s:%s (END LINE ADJUSTED)", cppRange, inoPath, inoRange)
			err = nil
		} else {
			logger.Logf("    URI: ERROR: %s", err)
			handler.sketchMapper.DebugLogAll()
			return lsp.NilURI, lsp.NilRange, err
		}
		inoURI, err := handler.inoDocumentURIFromInoPath(logger, inoPath)
		return inoURI, inoRange, err
	}

	inside, err := cppPath.IsInsideDir(handler.buildSketchRoot)
	if err != nil {
		logger.Logf("    could not determine if '%s' is inside '%s'", cppPath, handler.buildSketchRoot)
		return lsp.NilURI, lsp.NilRange, err
	}
	if !inside {
		logger.Logf("    '%s' is not inside '%s'", cppPath, handler.buildSketchRoot)
		logger.Logf("    keep doc identifier to '%s' as-is", cppPath)
		return cppURI, cppRange, nil
	}

	rel, err := handler.buildSketchRoot.RelTo(cppPath)
	if err == nil {
		inoPath := handler.sketchRoot.JoinPath(rel).String()
		logger.Logf("    URI: '%s' -> '%s'", cppPath, inoPath)
		inoURI, err := handler.inoDocumentURIFromInoPath(logger, inoPath)
		logger.Logf("              as URI: '%s'", inoURI)
		return inoURI, cppRange, err
	}

	logger.Logf("    could not determine rel-path of '%s' in '%s': %s", cppPath, handler.buildSketchRoot, err)
	return lsp.NilURI, lsp.NilRange, err
}

func (handler *INOLanguageServer) ino2cppTextDocumentPositionParams(logger jsonrpc.FunctionLogger, inoParams lsp.TextDocumentPositionParams) (lsp.TextDocumentPositionParams, error) {
	inoTextDocument := inoParams.TextDocument
	inoPosition := inoParams.Position
	inoURI := inoTextDocument.URI
	prefix := fmt.Sprintf("TextDocumentIdentifier %s", inoParams)

	cppTextDocument, err := handler.ino2cppTextDocumentIdentifier(logger, inoTextDocument)
	if err != nil {
		logger.Logf("%s -> invalid text document: %s", prefix, err)
		return lsp.TextDocumentPositionParams{}, err
	}
	cppPosition := inoPosition
	if inoURI.Ext() == ".ino" {
		if cppLine, ok := handler.sketchMapper.InoToCppLineOk(inoURI, inoPosition.Line); ok {
			cppPosition.Line = cppLine
		} else {
			logger.Logf("%s -> invalid line requested: %s:%d", prefix, inoURI, inoPosition.Line)
			return lsp.TextDocumentPositionParams{}, unknownURI(inoURI)
		}
	}
	cppParams := lsp.TextDocumentPositionParams{
		TextDocument: cppTextDocument,
		Position:     cppPosition,
	}
	logger.Logf("%s -> %s", prefix, cppParams)
	return cppParams, nil
}

func (handler *INOLanguageServer) ino2cppRange(logger jsonrpc.FunctionLogger, inoURI lsp.DocumentURI, inoRange lsp.Range) (lsp.DocumentURI, lsp.Range, error) {
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

func (handler *INOLanguageServer) ino2cppDocumentRangeFormattingParams(logger jsonrpc.FunctionLogger, inoParams *lsp.DocumentRangeFormattingParams) (*lsp.DocumentRangeFormattingParams, error) {
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

// func (handler *INOLanguageServer) ino2cppDocumentOnTypeFormattingParams(params *lsp.DocumentOnTypeFormattingParams) error {
// 	panic("not implemented")
// handler.sketchToBuildPathTextDocumentIdentifier(&params.TextDocument)
// if data, ok := handler.data[params.TextDocument.URI]; ok {
// 	params.Position.Line = data.sourceMap.InoToCppLine(data.sourceURI, params.Position.Line)
// 	return nil
// }
// return unknownURI(params.TextDocument.URI)
// }

// func (handler *INOLanguageServer) ino2cppRenameParams(params *lsp.RenameParams) error {
// handler.sketchToBuildPathTextDocumentIdentifier(&params.TextDocument)
// if data, ok := handler.data[params.TextDocument.URI]; ok {
// 	params.Position.Line = data.sourceMap.InoToCppLine(data.sourceURI, params.Position.Line)
// 	return nil
// }
// return unknownURI(params.TextDocument.URI)
// }

// func (handler *INOLanguageServer) ino2cppDidChangeWatchedFilesParams(params *lsp.DidChangeWatchedFilesParams) error {
// for index := range params.Changes {
// 	fileEvent := &params.Changes[index]
// 	if data, ok := handler.data[fileEvent.URI]; ok {
// 		fileEvent.URI = data.targetURI
// 	}
// }
// return nil
// }

// func (handler *INOLanguageServer) ino2cppExecuteCommand(executeCommand *lsp.ExecuteCommandParams) error {
// if len(executeCommand.Arguments) == 1 {
// 	arg := handler.parseCommandArgument(executeCommand.Arguments[0])
// 	if workspaceEdit, ok := arg.(*lsp.WorkspaceEdit); ok {
// 		executeCommand.Arguments[0] = handler.ino2cppWorkspaceEdit(workspaceEdit)
// 	}
// }
// return nil
// }

// func (handler *INOLanguageServer) ino2cppWorkspaceEdit(origEdit *lsp.WorkspaceEdit) *lsp.WorkspaceEdit {
// newEdit := lsp.WorkspaceEdit{}
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
// return &newEdit
// }

func (handler *INOLanguageServer) cpp2inoCodeAction(logger jsonrpc.FunctionLogger, codeAction lsp.CodeAction, uri lsp.DocumentURI) lsp.CodeAction {
	inoCodeAction := lsp.CodeAction{
		Title:       codeAction.Title,
		Kind:        codeAction.Kind,
		Edit:        handler.cpp2inoWorkspaceEdit(logger, codeAction.Edit),
		Diagnostics: codeAction.Diagnostics,
	}
	if codeAction.Command != nil {
		inoCommand := handler.cpp2inoCommand(logger, *codeAction.Command)
		inoCodeAction.Command = &inoCommand
	}
	if uri.Ext() == ".ino" {
		for i, diag := range inoCodeAction.Diagnostics {
			_, inoCodeAction.Diagnostics[i].Range = handler.sketchMapper.CppToInoRange(diag.Range)
		}
	}
	return inoCodeAction
}

func (handler *INOLanguageServer) cpp2inoCommand(logger jsonrpc.FunctionLogger, command lsp.Command) lsp.Command {
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
					logger.Logf("            > converted clangd ExtractVariable")
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

func (handler *INOLanguageServer) cpp2inoWorkspaceEdit(logger jsonrpc.FunctionLogger, cppWorkspaceEdit *lsp.WorkspaceEdit) *lsp.WorkspaceEdit {
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
				logger.Logf("    error converting edit %s:%s: %s", editURI, edit.Range, err)
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
	logger.Logf("    done converting workspaceEdit")
	return inoWorkspaceEdit
}

func (handler *INOLanguageServer) cpp2inoLocation(logger jsonrpc.FunctionLogger, cppLocation lsp.Location) (lsp.Location, error) {
	inoURI, inoRange, err := handler.cpp2inoDocumentURI(logger, cppLocation.URI, cppLocation.Range)
	return lsp.Location{
		URI:   inoURI,
		Range: inoRange,
	}, err
}

func (handler *INOLanguageServer) cpp2inoDocumentHighlight(logger jsonrpc.FunctionLogger, cppHighlight lsp.DocumentHighlight, cppURI lsp.DocumentURI) (lsp.DocumentHighlight, error) {
	_, inoRange, err := handler.cpp2inoDocumentURI(logger, cppURI, cppHighlight.Range)
	if err != nil {
		return lsp.DocumentHighlight{}, err
	}
	return lsp.DocumentHighlight{
		Kind:  cppHighlight.Kind,
		Range: inoRange,
	}, nil
}

func (handler *INOLanguageServer) cpp2inoTextEdits(logger jsonrpc.FunctionLogger, cppURI lsp.DocumentURI, cppEdits []lsp.TextEdit) (map[lsp.DocumentURI][]lsp.TextEdit, error) {
	logger.Logf("%s cpp/textEdit (%d elements)", cppURI, len(cppEdits))
	allInoEdits := map[lsp.DocumentURI][]lsp.TextEdit{}
	for _, cppEdit := range cppEdits {
		logger.Logf("        > %s -> %s", cppEdit.Range, strconv.Quote(cppEdit.NewText))
		inoURI, inoEdit, err := handler.cpp2inoTextEdit(logger, cppURI, cppEdit)
		if err != nil {
			return nil, err
		}
		allInoEdits[inoURI] = append(allInoEdits[inoURI], inoEdit)
	}

	logger.Logf("converted to:")

	for inoURI, inoEdits := range allInoEdits {
		logger.Logf("-> %s ino/textEdit (%d elements)", inoURI, len(inoEdits))
		for _, inoEdit := range inoEdits {
			logger.Logf("        > %s -> %s", inoEdit.Range, strconv.Quote(inoEdit.NewText))
		}
	}
	return allInoEdits, nil
}

func (handler *INOLanguageServer) cpp2inoTextEdit(logger jsonrpc.FunctionLogger, cppURI lsp.DocumentURI, cppEdit lsp.TextEdit) (lsp.DocumentURI, lsp.TextEdit, error) {
	inoURI, inoRange, err := handler.cpp2inoDocumentURI(logger, cppURI, cppEdit.Range)
	inoEdit := cppEdit
	inoEdit.Range = inoRange
	return inoURI, inoEdit, err
}

func (handler *INOLanguageServer) cpp2inoDocumentSymbols(logger jsonrpc.FunctionLogger, cppSymbols []lsp.DocumentSymbol, inoRequestedURI lsp.DocumentURI) []lsp.DocumentSymbol {
	inoRequested := inoRequestedURI.AsPath().String()
	logger.Logf("    filtering for requested ino file: %s", inoRequested)
	if inoRequestedURI.Ext() != ".ino" || len(cppSymbols) == 0 {
		return cppSymbols
	}

	inoSymbols := []lsp.DocumentSymbol{}
	for _, symbol := range cppSymbols {
		logger.Logf("    > convert %s %s", symbol.Kind, symbol.Range)
		if handler.sketchMapper.IsPreprocessedCppLine(symbol.Range.Start.Line) {
			logger.Logf("      symbol is in the preprocessed section of the sketch.ino.cpp")
			continue
		}

		inoFile, inoRange := handler.sketchMapper.CppToInoRange(symbol.Range)
		inoSelectionURI, inoSelectionRange := handler.sketchMapper.CppToInoRange(symbol.SelectionRange)

		if inoFile != inoSelectionURI {
			logger.Logf("      ERROR: symbol range and selection belongs to different URI!")
			logger.Logf("        symbol %s != selection %s", symbol.Range, symbol.SelectionRange)
			logger.Logf("        %s:%s != %s:%s", inoFile, inoRange, inoSelectionURI, inoSelectionRange)
			continue
		}

		if inoFile != inoRequested {
			logger.Logf("    skipping symbol related to %s", inoFile)
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

func (handler *INOLanguageServer) cpp2inoSymbolInformation(syms []lsp.SymbolInformation) []lsp.SymbolInformation {
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

func (handler *INOLanguageServer) cpp2inoDiagnostics(logger jsonrpc.FunctionLogger, cppDiags *lsp.PublishDiagnosticsParams) ([]*lsp.PublishDiagnosticsParams, error) {
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
					handler.queueCheckCppDocumentSymbols()
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

// // HandleRequestFromClangd handles a request message received from clangd.
// func (handler *INOLanguageServer) HandleRequestFromClangd(ctx context.Context, logger jsonrpc.FunctionLogger,
// 	method string, paramsRaw json.RawMessage,
// 	respCallback func(result json.RawMessage, err *jsonrpc.ResponseError),
// ) {
// 	// n := atomic.AddInt64(&handler.clangdMessageCount, 1)
// 	// prefix := fmt.Sprintf("CLG <-- %s %v ", method, n)
// 	params, err := lsp.DecodeServerRequestParams(method, paramsRaw)
// 	if err != nil {
// 		logger.Logf("Error parsing clang message: %v", err)
// 		respCallback(nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()})
// 		return
// 	}
// 	panic("unimplemented")
// 	// Default to read lock
// 	handler.readLock(logger, false)
// 	defer handler.readUnlock(logger)
// 	switch p := params.(type) {
// 	case *lsp.ApplyWorkspaceEditParams:
// 		// "workspace/applyEdit"
// 		p.Edit = *handler.cpp2inoWorkspaceEdit(logger, &p.Edit)
// 	}
// 	if err != nil {
// 		logger.Logf("From clangd: Method: %s, Error: %v", method, err)
// 		respCallback(nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()})
// 		return
// 	}
// 	// respRaw := lsp.EncodeMessage(params)
// 	// if params == nil {
// 	// 	// passthrough
// 	// 	logger.Logf("passing through message")
// 	// 	respRaw = paramsRaw
// 	// }

// 	// logger.Logf("IDE <-- LS REQ %s", method)
// 	// resp, respErr, err := handler.IDEConn.SendRequest(ctx, method, respRaw)
// 	// if err != nil {
// 	// 	logger.Logf("Error sending request to IDE:", err)
// 	// 	respCallback(nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()})
// 	// 	return
// 	// }
// 	// logger.Logf("IDE --> LS REQ %s", method)
// 	// respCallback(resp, respErr)
// }

func (handler *INOLanguageServer) createClangdFormatterConfig(logger jsonrpc.FunctionLogger, cppuri lsp.DocumentURI) (func(), error) {
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
			logger.Logf("    error reading custom formatter config file %s: %s", conf, err)
		} else {
			logger.Logf("    using custom formatter config file %s", conf)
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
		logger.Logf("    formatter config cleaned")
	}
	logger.Logf("    writing formatter config in: %s", targetFile)
	err := targetFile.WriteFile([]byte(config))
	return cleanup, err
}

func (handler *INOLanguageServer) showMessage(logger jsonrpc.FunctionLogger, msgType lsp.MessageType, message string) {
	params := lsp.ShowMessageParams{
		Type:    msgType,
		Message: message,
	}
	if err := handler.IDEConn.WindowShowMessage(&params); err != nil {
		logger.Logf("error sending showMessage notification: %s", err)
	}
}

func unknownURI(uri lsp.DocumentURI) error {
	return errors.New("Document is not available: " + uri.String())
}
