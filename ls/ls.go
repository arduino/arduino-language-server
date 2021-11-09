package ls

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arduino/arduino-cli/executils"
	"github.com/arduino/arduino-language-server/sourcemapper"
	"github.com/arduino/arduino-language-server/streams"
	"github.com/arduino/arduino-language-server/textutils"
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
	IDE    *IDELSPServer
	Clangd *ClangdLSPClient

	progressHandler *ProgressProxyHandler

	closing                    chan bool
	clangdStarted              *sync.Cond
	dataMux                    sync.RWMutex
	lspInitializeParams        *lsp.InitializeParams
	buildPath                  *paths.Path
	buildSketchRoot            *paths.Path
	buildSketchCpp             *paths.Path
	buildSketchSymbols         []lsp.DocumentSymbol
	buildSketchIncludesCanary  string
	buildSketchSymbolsCanary   string
	buildSketchSymbolsLoad     chan bool
	buildSketchSymbolsCheck    chan bool
	rebuildSketchDeadline      *time.Time
	rebuildSketchDeadlineMutex sync.Mutex
	sketchRoot                 *paths.Path
	sketchName                 string
	sketchMapper               *sourcemapper.SketchMapper
	sketchTrackedFilesCount    int
	trackedInoDocs             map[string]lsp.TextDocumentItem
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

func (ls *INOLanguageServer) writeLock(logger jsonrpc.FunctionLogger, requireClangd bool) {
	ls.dataMux.Lock()
	logger.Logf(yellow.Sprintf("write-locked"))
	if requireClangd {
		ls.waitClangdStart(logger)
	}
}

func (ls *INOLanguageServer) writeUnlock(logger jsonrpc.FunctionLogger) {
	logger.Logf(yellow.Sprintf("write-unlocked"))
	ls.dataMux.Unlock()
}

func (ls *INOLanguageServer) readLock(logger jsonrpc.FunctionLogger, requireClangd bool) {
	ls.dataMux.RLock()
	logger.Logf(yellow.Sprintf("read-locked"))

	for requireClangd && ls.Clangd == nil {
		// if clangd is not started...

		// Release the read lock and acquire a write lock
		// (this is required to wait on condition variable and restart clang).
		logger.Logf(yellow.Sprintf("clang not started: read-unlocking..."))
		ls.dataMux.RUnlock()

		ls.writeLock(logger, true)
		ls.writeUnlock(logger)

		ls.dataMux.RLock()
		logger.Logf(yellow.Sprintf("testing again if clang started: read-locked..."))
	}
}

func (ls *INOLanguageServer) readUnlock(logger jsonrpc.FunctionLogger) {
	logger.Logf(yellow.Sprintf("read-unlocked"))
	ls.dataMux.RUnlock()
}

func (ls *INOLanguageServer) waitClangdStart(logger jsonrpc.FunctionLogger) error {
	if ls.Clangd != nil {
		return nil
	}

	logger.Logf("(throttled: waiting for clangd)")
	logger.Logf(yellow.Sprintf("unlocked (waiting clangd)"))
	ls.clangdStarted.Wait()
	logger.Logf(yellow.Sprintf("locked (waiting clangd)"))

	if ls.Clangd == nil {
		logger.Logf("clangd startup failed: aborting call")
		return errors.New("could not start clangd, aborted")
	}
	return nil
}

// NewINOLanguageServer creates and configures an Arduino Language Server.
func NewINOLanguageServer(stdin io.Reader, stdout io.Writer, board Board) *INOLanguageServer {
	logger := NewLSPFunctionLogger(color.HiWhiteString, "LS: ")
	ls := &INOLanguageServer{
		trackedInoDocs:          map[string]lsp.TextDocumentItem{},
		inoDocsWithDiagnostics:  map[lsp.DocumentURI]bool{},
		closing:                 make(chan bool),
		buildSketchSymbolsLoad:  make(chan bool, 1),
		buildSketchSymbolsCheck: make(chan bool, 1),
		config: BoardConfig{
			SelectedBoard: board,
		},
	}
	ls.clangdStarted = sync.NewCond(&ls.dataMux)

	if buildPath, err := paths.MkTempDir("", "arduino-language-server"); err != nil {
		log.Fatalf("Could not create temp folder: %s", err)
	} else {
		ls.buildPath = buildPath.Canonical()
		ls.buildSketchRoot = ls.buildPath.Join("sketch")
	}
	if enableLogging {
		logger.Logf("Initial board configuration: %s", board)
		logger.Logf("Language server build path: %s", ls.buildPath)
		logger.Logf("Language server build sketch root: %s", ls.buildSketchRoot)
	}

	ls.IDE = NewIDELSPServer(logger, stdin, stdout, ls)
	ls.progressHandler = NewProgressProxy(ls.IDE.conn)
	go func() {
		defer streams.CatchAndLogPanic()
		ls.IDE.Run()
		logger.Logf("Lost connection with IDE!")
		ls.Close()
	}()

	go func() {
		defer streams.CatchAndLogPanic()
		for {
			select {
			case <-ls.buildSketchSymbolsLoad:
				// ...also un-queue buildSketchSymbolsCheck
				select {
				case <-ls.buildSketchSymbolsCheck:
				default:
				}
				ls.LoadCppDocumentSymbols()

			case <-ls.buildSketchSymbolsCheck:
				ls.CheckCppDocumentSymbols()

			case <-ls.closing:
				return
			}
		}
	}()

	go func() {
		defer streams.CatchAndLogPanic()
		ls.rebuildEnvironmentLoop()
	}()
	return ls
}

func (ls *INOLanguageServer) queueLoadCppDocumentSymbols() {
	select {
	case ls.buildSketchSymbolsLoad <- true:
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

func (handler *INOLanguageServer) InitializeReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.InitializeParams) (*lsp.InitializeResult, *jsonrpc.ResponseError) {
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
			CompletionProvider: &lsp.CompletionOptions{
				AllCommitCharacters: []string{
					" ", "\t", "(", ")", "[", "]", "{", "}", "<", ">",
					":", ";", ",", "+", "-", "/", "*", "%", "^", "&",
					"#", "?", ".", "=", "\"", "'", "|"},
				ResolveProvider: false,
				TriggerCharacters: []string{ //".", "\u003e", ":"
					".", "<", ">", ":", "\"", "/"},
			},
			SignatureHelpProvider: &lsp.SignatureHelpOptions{
				TriggerCharacters: []string{"(", ","},
			},
			// ReferencesProvider:              &lsp.ReferenceOptions{},
			// DeclarationProvider:             &lsp.DeclarationRegistrationOptions{},
			// DocumentLinkProvider:            &lsp.DocumentLinkOptions{ResolveProvider: false},
			// ImplementationProvider:          &lsp.ImplementationRegistrationOptions{},
			// SelectionRangeProvider:          &lsp.SelectionRangeRegistrationOptions{},
			DefinitionProvider:              &lsp.DefinitionOptions{},
			DocumentHighlightProvider:       &lsp.DocumentHighlightOptions{},
			DocumentSymbolProvider:          &lsp.DocumentSymbolOptions{},
			WorkspaceSymbolProvider:         &lsp.WorkspaceSymbolOptions{},
			CodeActionProvider:              &lsp.CodeActionOptions{ResolveProvider: true},
			DocumentFormattingProvider:      &lsp.DocumentFormattingOptions{},
			DocumentRangeFormattingProvider: &lsp.DocumentRangeFormattingOptions{},
			HoverProvider:                   &lsp.HoverOptions{},
			DocumentOnTypeFormattingProvider: &lsp.DocumentOnTypeFormattingOptions{
				FirstTriggerCharacter: "\n",
				MoreTriggerCharacter:  []string{},
			},
			RenameProvider: &lsp.RenameOptions{
				// PrepareProvider: true,
			},
			ExecuteCommandProvider: &lsp.ExecuteCommandOptions{
				Commands: []string{"clangd.applyFix", "clangd.applyTweak"},
			},
			// SemanticTokensProvider: &lsp.SemanticTokensRegistrationOptions{
			// 	SemanticTokensOptions: &lsp.SemanticTokensOptions{
			// 		Full: &lsp.SemantiTokenFullOptions{
			// 			Delta: true,
			// 		},
			// 		Legend: lsp.SemanticTokensLegend{
			// 			TokenModifiers: []string{},
			// 			TokenTypes: []string{
			// 				"variable", "variable", "parameter", "function", "method", "function", "property", "variable",
			// 				"class", "enum", "enumMember", "type", "dependent", "dependent", "namespace", "typeParameter",
			// 				"concept", "type", "macro", "comment",
			// 			},
			// 		},
			// 		Range: false,
			// 	},
			// },
		},
		ServerInfo: &lsp.InitializeResultServerInfo{
			Name:    "arduino-language-server",
			Version: "0.5.0-beta",
		},
	}
	logger.Logf("initialization parameters: %s", string(lsp.EncodeMessage(resp)))
	return resp, nil
}

func (ls *INOLanguageServer) ShutdownReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger) *jsonrpc.ResponseError {
	logger.Logf("Sending shutdown notification to clangd...")
	ls.Clangd.conn.Shutdown(context.Background())
	logger.Logf("Arduino Language Server is shutting down.")
	ls.Close()
	return nil
}

func (ls *INOLanguageServer) TextDocumentCompletionReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.CompletionParams) (*lsp.CompletionList, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	logger.Logf("--> completion(%s)\n", inoParams.TextDocument)
	cppTextDocPositionParams, err := ls.ino2cppTextDocumentPositionParams(logger, inoParams.TextDocumentPositionParams)
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

	clangResp, clangErr, err := ls.Clangd.conn.TextDocumentCompletion(ctx, cppParams)
	if err != nil {
		logger.Logf("clangd connection error: %v", err)
		ls.Close()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response error: %v", clangErr.AsError())
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	cppToIno := inoURI != lsp.NilURI && inoURI.AsPath().EquivalentTo(ls.buildSketchCpp)

	inoResp := *clangResp
	inoItems := make([]lsp.CompletionItem, 0)
	for _, item := range clangResp.Items {
		if !strings.HasPrefix(item.InsertText, "_") {
			if cppToIno && item.TextEdit != nil {
				_, item.TextEdit.Range = ls.sketchMapper.CppToInoRange(item.TextEdit.Range)
			}
			inoItems = append(inoItems, item)
		}
	}
	inoResp.Items = inoItems
	logger.Logf("<-- completion(%d items) cppToIno=%v", len(inoResp.Items), cppToIno)
	return &inoResp, nil
}

func (ls *INOLanguageServer) TextDocumentHoverReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.HoverParams) (*lsp.Hover, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	inoURI := inoParams.TextDocument.URI
	inoTextDocPosition := inoParams.TextDocumentPositionParams
	logger.Logf("--> hover(%s)\n", inoTextDocPosition)

	cppTextDocPosition, err := ls.ino2cppTextDocumentPositionParams(logger, inoTextDocPosition)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	logger.Logf("    --> hover(%s)\n", cppTextDocPosition)
	cppParams := &lsp.HoverParams{
		TextDocumentPositionParams: cppTextDocPosition,
	}
	clangResp, clangErr, err := ls.Clangd.conn.TextDocumentHover(ctx, cppParams)
	if err != nil {
		logger.Logf("clangd connectiono error: %v", err)
		ls.Close()
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
	cppToIno := inoURI != lsp.NilURI && inoURI.AsPath().EquivalentTo(ls.buildSketchCpp)
	if cppToIno {
		_, inoRange := ls.sketchMapper.CppToInoRange(*clangResp.Range)
		inoResp.Range = &inoRange
	}
	logger.Logf("<-- hover(%s)", strconv.Quote(inoResp.Contents.Value))
	return &inoResp, nil
}

func (ls *INOLanguageServer) TextDocumentSignatureHelpReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.SignatureHelpParams) (*lsp.SignatureHelp, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	inoTextDocumentPosition := inoParams.TextDocumentPositionParams

	logger.Logf("%s", inoTextDocumentPosition)
	cppTextDocumentPosition, err := ls.ino2cppTextDocumentPositionParams(logger, inoTextDocumentPosition)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	logger.Logf("-> %s", cppTextDocumentPosition)
	cppParams := *inoParams
	cppParams.TextDocumentPositionParams = cppTextDocumentPosition
	cppSignatureHelp, cppErr, err := ls.Clangd.conn.TextDocumentSignatureHelp(ctx, inoParams)
	if err != nil {
		logger.Logf("clangd connectiono error: %v", err)
		ls.Close()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if cppErr != nil {
		logger.Logf("clangd response error: %v", cppErr.AsError())
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: cppErr.AsError().Error()}
	}

	// No need to convert back to inoSignatureHelp

	return cppSignatureHelp, nil
}

func (ls *INOLanguageServer) TextDocumentDefinitionReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, p *lsp.DefinitionParams) ([]lsp.Location, []lsp.LocationLink, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	inoTextDocPosition := p.TextDocumentPositionParams

	logger.Logf("%s", inoTextDocPosition)
	cppTextDocPosition, err := ls.ino2cppTextDocumentPositionParams(logger, inoTextDocPosition)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	logger.Logf("-> %s", cppTextDocPosition)
	cppParams := *p
	cppParams.TextDocumentPositionParams = cppTextDocPosition
	cppLocations, cppLocationLinks, cppErr, err := ls.Clangd.conn.TextDocumentDefinition(ctx, &cppParams)
	if err != nil {
		logger.Logf("clangd connectiono error: %v", err)
		ls.Close()
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if cppErr != nil {
		logger.Logf("clangd response error: %v", cppErr.AsError())
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: cppErr.AsError().Error()}
	}

	var inoLocations []lsp.Location
	if cppLocations != nil {
		inoLocations, err = ls.cpp2inoLocationArray(logger, cppLocations)
		if err != nil {
			ls.Close()
			return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
		}
	}

	var inoLocationLinks []lsp.LocationLink
	if cppLocationLinks != nil {
		panic("unimplemented")
	}

	return inoLocations, inoLocationLinks, nil
}

func (ls *INOLanguageServer) TextDocumentTypeDefinitionReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.TypeDefinitionParams) ([]lsp.Location, []lsp.LocationLink, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	inoTextDocumentPosition := inoParams.TextDocumentPositionParams

	logger.Logf("%s", inoTextDocumentPosition)
	// inoURI := inoTextDocumentPosition.TextDocument.URI
	cppTextDocumentPosition, err := ls.ino2cppTextDocumentPositionParams(logger, inoTextDocumentPosition)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	// cppURI := cppTextDocumentPosition.TextDocument.URI
	logger.Logf("-> %s", cppTextDocumentPosition)

	cppParams := *inoParams
	cppParams.TextDocumentPositionParams = cppTextDocumentPosition
	cppLocations, cppLocationLinks, cppErr, err := ls.Clangd.conn.TextDocumentTypeDefinition(ctx, &cppParams)
	if err != nil {
		logger.Logf("clangd connectiono error: %v", err)
		ls.Close()
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if cppErr != nil {
		logger.Logf("clangd response error: %v", cppErr.AsError())
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: cppErr.AsError().Error()}
	}

	var inoLocations []lsp.Location
	if cppLocations != nil {
		inoLocations, err = ls.cpp2inoLocationArray(logger, cppLocations)
		if err != nil {
			ls.Close()
			return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
		}
	}

	var inoLocationLinks []lsp.LocationLink
	if cppLocationLinks != nil {
		panic("unimplemented")
	}

	return inoLocations, inoLocationLinks, nil
}

func (ls *INOLanguageServer) TextDocumentImplementationReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.ImplementationParams) ([]lsp.Location, []lsp.LocationLink, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	inoTextDocumentPosition := inoParams.TextDocumentPositionParams
	logger.Logf("%s", inoTextDocumentPosition)

	cppTextDocumentPosition, err := ls.ino2cppTextDocumentPositionParams(logger, inoTextDocumentPosition)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	logger.Logf("-> %s", cppTextDocumentPosition)
	cppParams := *inoParams
	cppParams.TextDocumentPositionParams = cppTextDocumentPosition
	cppLocations, cppLocationLinks, cppErr, err := ls.Clangd.conn.TextDocumentImplementation(ctx, &cppParams)
	if err != nil {
		logger.Logf("clangd connectiono error: %v", err)
		ls.Close()
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if cppErr != nil {
		logger.Logf("clangd response error: %v", cppErr.AsError())
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: cppErr.AsError().Error()}
	}

	var inoLocations []lsp.Location
	if cppLocations != nil {
		inoLocations, err = ls.cpp2inoLocationArray(logger, cppLocations)
		if err != nil {
			ls.Close()
			return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
		}
	}

	var inoLocationLinks []lsp.LocationLink
	if cppLocationLinks != nil {
		panic("unimplemented")
	}

	return inoLocations, inoLocationLinks, nil
}

func (ls *INOLanguageServer) TextDocumentDocumentHighlightReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.DocumentHighlightParams) ([]lsp.DocumentHighlight, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	inoTextDocumentPosition := inoParams.TextDocumentPositionParams
	cppTextDocumentPosition, err := ls.ino2cppTextDocumentPositionParams(logger, inoTextDocumentPosition)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	cppURI := cppTextDocumentPosition.TextDocument.URI

	cppParams := *inoParams
	cppParams.TextDocumentPositionParams = cppTextDocumentPosition
	cppHighlights, clangErr, err := ls.Clangd.conn.TextDocumentDocumentHighlight(ctx, &cppParams)
	if err != nil {
		logger.Logf("clangd connectiono error: %v", err)
		ls.Close()
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
		inoHighlight, err := ls.cpp2inoDocumentHighlight(logger, cppHighlight, cppURI)
		if err != nil {
			logger.Logf("ERROR converting location %s:%s: %s", cppURI, cppHighlight.Range, err)
			return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
		}
		inoHighlights = append(inoHighlights, inoHighlight)
	}
	return inoHighlights, nil
}

func (ls *INOLanguageServer) TextDocumentDocumentSymbolReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.DocumentSymbolParams) ([]lsp.DocumentSymbol, []lsp.SymbolInformation, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	inoTextDocument := inoParams.TextDocument
	inoURI := inoTextDocument.URI
	logger.Logf("--> %s")

	cppTextDocument, err := ls.ino2cppTextDocumentIdentifier(logger, inoTextDocument)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	cppParams := *inoParams
	cppParams.TextDocument = cppTextDocument
	logger.Logf("    --> documentSymbol(%s)", cppTextDocument)
	cppDocSymbols, cppSymbolInformation, clangErr, err := ls.Clangd.conn.TextDocumentDocumentSymbol(ctx, &cppParams)
	if err != nil {
		logger.Logf("clangd connectiono error: %v", err)
		ls.Close()
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response error: %v", clangErr.AsError())
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	var inoDocSymbols []lsp.DocumentSymbol
	if cppDocSymbols != nil {
		logger.Logf("    <-- documentSymbol(%d document symbols)", len(cppDocSymbols))
		inoDocSymbols = ls.cpp2inoDocumentSymbols(logger, cppDocSymbols, inoURI)
	}
	var inoSymbolInformation []lsp.SymbolInformation
	if cppSymbolInformation != nil {
		logger.Logf("    <-- documentSymbol(%d symbol information)", len(cppSymbolInformation))
		inoSymbolInformation = ls.cpp2inoSymbolInformation(cppSymbolInformation)
	}
	return inoDocSymbols, inoSymbolInformation, nil
}

func (ls *INOLanguageServer) TextDocumentCodeActionReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.CodeActionParams) ([]lsp.CommandOrCodeAction, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	inoTextDocument := inoParams.TextDocument
	inoURI := inoTextDocument.URI
	logger.Logf("--> codeAction(%s:%s)", inoTextDocument, inoParams.Range.Start)

	cppParams := *inoParams
	cppTextDocument, err := ls.ino2cppTextDocumentIdentifier(logger, inoTextDocument)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	cppParams.TextDocument = cppTextDocument

	if cppTextDocument.URI.AsPath().EquivalentTo(ls.buildSketchCpp) {
		cppParams.Range = ls.sketchMapper.InoToCppLSPRange(inoURI, inoParams.Range)
		for i, inoDiag := range inoParams.Context.Diagnostics {
			cppParams.Context.Diagnostics[i].Range = ls.sketchMapper.InoToCppLSPRange(inoURI, inoDiag.Range)
		}
	}
	logger.Logf("    --> codeAction(%s:%s)", cppParams.TextDocument, inoParams.Range.Start)

	cppResp, cppErr, err := ls.Clangd.conn.TextDocumentCodeAction(ctx, &cppParams)
	if err != nil {
		logger.Logf("clangd connectiono error: %v", err)
		ls.Close()
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
				inoItem.Set(ls.cpp2inoCommand(logger, i))
			case lsp.CodeAction:
				logger.Logf("        > CodeAction: %s", i.Title)
				inoItem.Set(ls.cpp2inoCodeAction(logger, i, inoURI))
			}
			inoResp = append(inoResp, inoItem)
		}
		logger.Logf("<-- codeAction(%d elements)", len(inoResp))
	}
	return inoResp, nil
}

func (ls *INOLanguageServer) TextDocumentFormattingReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.DocumentFormattingParams) ([]lsp.TextEdit, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	inoTextDocument := inoParams.TextDocument
	inoURI := inoTextDocument.URI
	logger.Logf("--> formatting(%s)", inoTextDocument)

	cppTextDocument, err := ls.ino2cppTextDocumentIdentifier(logger, inoTextDocument)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	cppURI := cppTextDocument.URI

	logger.Logf("    --> formatting(%s)", cppTextDocument)

	if cleanup, e := ls.createClangdFormatterConfig(logger, cppURI); e != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	} else {
		defer cleanup()
	}

	cppParams := *inoParams
	cppParams.TextDocument = cppTextDocument
	cppEdits, clangErr, err := ls.Clangd.conn.TextDocumentFormatting(ctx, &cppParams)
	if err != nil {
		logger.Logf("clangd connectiono error: %v", err)
		ls.Close()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response error: %v", clangErr.AsError())
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	if cppEdits == nil {
		return nil, nil
	}

	sketchEdits, err := ls.cpp2inoTextEdits(logger, cppURI, cppEdits)
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

func (ls *INOLanguageServer) TextDocumentRangeFormattingReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.DocumentRangeFormattingParams) ([]lsp.TextEdit, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	// Method: "textDocument/rangeFormatting"
	logger.Logf("%s", inoParams.TextDocument)
	inoURI := inoParams.TextDocument.URI
	cppParams, err := ls.ino2cppDocumentRangeFormattingParams(logger, inoParams)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	cppURI := cppParams.TextDocument.URI
	logger.Logf("-> %s", cppParams.TextDocument)
	if cleanup, e := ls.createClangdFormatterConfig(logger, cppURI); e != nil {
		logger.Logf("cannot create formatter config file: %v", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	} else {
		defer cleanup()
	}

	cppEdits, clangErr, err := ls.Clangd.conn.TextDocumentRangeFormatting(ctx, cppParams)
	if err != nil {
		logger.Logf("clangd connectiono error: %v", err)
		ls.Close()
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

	sketchEdits, err := ls.cpp2inoTextEdits(logger, cppURI, cppEdits)
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

func (ls *INOLanguageServer) InitializedNotifFromIDE(logger jsonrpc.FunctionLogger, params *lsp.InitializedParams) {
	logger.Logf("Notification is not propagated to clangd")
}

func (ls *INOLanguageServer) ExitNotifFromIDE(logger jsonrpc.FunctionLogger) {
	logger.Logf("Notification is not propagated to clangd")
}

func (ls *INOLanguageServer) TextDocumentDidOpenNotifFromIDE(logger jsonrpc.FunctionLogger, inoParam *lsp.DidOpenTextDocumentParams) {
	ls.writeLock(logger, true)
	defer ls.writeUnlock(logger)

	// Add the TextDocumentItem in the tracked files list
	inoTextDocItem := inoParam.TextDocument
	ls.trackedInoDocs[inoTextDocItem.URI.AsPath().String()] = inoTextDocItem

	// If we are tracking a .ino...
	if inoTextDocItem.URI.Ext() == ".ino" {
		ls.sketchTrackedFilesCount++
		logger.Logf("Increasing .ino tracked files count to %d", ls.sketchTrackedFilesCount)

		// Notify clangd that sketchCpp has been opened only once
		if ls.sketchTrackedFilesCount != 1 {
			logger.Logf("Clang already notified, do not notify it anymore")
			return
		}

		// Queue a load of ino.cpp document symbols
		ls.queueLoadCppDocumentSymbols()
	}

	if cppItem, err := ls.ino2cppTextDocumentItem(logger, inoTextDocItem); err != nil {
		logger.Logf("Error: %s", err)
	} else if err := ls.Clangd.conn.TextDocumentDidOpen(&lsp.DidOpenTextDocumentParams{
		TextDocument: cppItem,
	}); err != nil {
		// Exit the process and trigger a restart by the client in case of a severe error
		logger.Logf("Error sending notification to clangd server: %v", err)
		logger.Logf("Please restart the language server.")
		ls.Close()
	}
}

func (ls *INOLanguageServer) TextDocumentDidChangeNotifFromIDE(logger jsonrpc.FunctionLogger, inoParams *lsp.DidChangeTextDocumentParams) {
	ls.writeLock(logger, true)
	defer ls.writeUnlock(logger)

	logger.Logf("didChange(%s)", inoParams.TextDocument)
	for _, change := range inoParams.ContentChanges {
		logger.Logf("> %s", change)
	}

	if cppParams, err := ls.didChange(logger, inoParams); err != nil {
		logger.Logf("--E Error: %s", err)
	} else if cppParams == nil {
		logger.Logf("--X Notification is not propagated to clangd")
	} else {
		logger.Logf("LS --> CL NOTIF didChange(%s@%d)", cppParams.TextDocument)
		for _, change := range cppParams.ContentChanges {
			logger.Logf("                > %s", change)
		}
		if err := ls.Clangd.conn.TextDocumentDidChange(cppParams); err != nil {
			// Exit the process and trigger a restart by the client in case of a severe error
			logger.Logf("Connection error with clangd server: %v", err)
			logger.Logf("Please restart the language server.")
			ls.Close()
		}
	}
}

func (ls *INOLanguageServer) TextDocumentDidSaveNotifFromIDE(logger jsonrpc.FunctionLogger, inoParams *lsp.DidSaveTextDocumentParams) {
	ls.writeLock(logger, true)
	defer ls.writeUnlock(logger)

	logger.Logf("didSave(%s) hasText=%v", inoParams.TextDocument, inoParams.Text != "")
	if cppTextDocument, err := ls.ino2cppTextDocumentIdentifier(logger, inoParams.TextDocument); err != nil {
		logger.Logf("--E Error: %s", err)
	} else if cppTextDocument.URI.AsPath().EquivalentTo(ls.buildSketchCpp) {
		logger.Logf("    didSave(%s) equals %s", cppTextDocument, ls.buildSketchCpp)
		logger.Logf("    the notification will be not forwarded to clangd")
	} else {
		logger.Logf("LS --> CL NOTIF didSave(%s)", cppTextDocument)
		if err := ls.Clangd.conn.TextDocumentDidSave(&lsp.DidSaveTextDocumentParams{
			TextDocument: cppTextDocument,
			Text:         inoParams.Text,
		}); err != nil {
			// Exit the process and trigger a restart by the client in case of a severe error
			logger.Logf("Connection error with clangd server: %v", err)
			logger.Logf("Please restart the language server.")
			ls.Close()
		}
	}
}

func (ls *INOLanguageServer) TextDocumentDidCloseNotifFromIDE(logger jsonrpc.FunctionLogger, inoParams *lsp.DidCloseTextDocumentParams) {
	ls.writeLock(logger, true)
	defer ls.writeUnlock(logger)

	logger.Logf("didClose(%s)", inoParams.TextDocument)

	if cppParams, err := ls.didClose(logger, inoParams); err != nil {
		logger.Logf("--E Error: %s", err)
	} else if cppParams == nil {
		logger.Logf("--X Notification is not propagated to clangd")
	} else {
		logger.Logf("--> CL NOTIF didClose(%s)", cppParams.TextDocument)
		if err := ls.Clangd.conn.TextDocumentDidClose(cppParams); err != nil {
			// Exit the process and trigger a restart by the client in case of a severe error
			logger.Logf("Error sending notification to clangd server: %v", err)
			logger.Logf("Please restart the language server.")
			ls.Close()
		}
	}
}

func (ls *INOLanguageServer) PublishDiagnosticsNotifFromClangd(logger jsonrpc.FunctionLogger, cppParams *lsp.PublishDiagnosticsParams) {
	// Default to read lock
	ls.readLock(logger, false)
	defer ls.readUnlock(logger)

	logger.Logf("from clang %s (%d diagnostics):", cppParams.URI, cppParams.Diagnostics)
	for _, diag := range cppParams.Diagnostics {
		logger.Logf("> %d:%d - %v: %s", diag.Range.Start.Line, diag.Range.Start.Character, diag.Severity, diag.Code)
	}

	// the diagnostics on sketch.cpp.ino once mapped into their
	// .ino counter parts may span over multiple .ino files...
	allInoParams, err := ls.cpp2inoDiagnostics(logger, cppParams)
	if err != nil {
		logger.Logf("    Error converting diagnostics to .ino: %s", err)
		return
	}

	// Push back to IDE the converted diagnostics
	for _, inoParams := range allInoParams {
		logger.Logf("to IDE: %s (%d diagnostics):", inoParams.URI, len(inoParams.Diagnostics))
		for _, diag := range inoParams.Diagnostics {
			logger.Logf("        > %d:%d - %v: %s", diag.Range.Start.Line, diag.Range.Start.Character, diag.Severity, diag.Code)
		}
		if err := ls.IDE.conn.TextDocumentPublishDiagnostics(inoParams); err != nil {
			logger.Logf("    Error sending diagnostics to IDE: %s", err)
			return
		}
	}
}

func (ls *INOLanguageServer) ProgressNotifFromClangd(logger jsonrpc.FunctionLogger, progress *lsp.ProgressParams) {
	var token string
	if err := json.Unmarshal(progress.Token, &token); err != nil {
		logger.Logf("error decoding progess token: %s", err)
		return
	}
	switch value := progress.TryToDecodeWellKnownValues().(type) {
	case lsp.WorkDoneProgressBegin:
		logger.Logf("%s %s", token, value)
		ls.progressHandler.Begin(token, &value)
	case lsp.WorkDoneProgressReport:
		logger.Logf("%s %s", token, value)
		ls.progressHandler.Report(token, &value)
	case lsp.WorkDoneProgressEnd:
		logger.Logf("%s %s", token, value)
		ls.progressHandler.End(token, &value)
	default:
		logger.Logf("error unsupported $/progress: " + string(progress.Value))
	}
}

func (ls *INOLanguageServer) WindowWorkDoneProgressCreateReqFromClangd(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.WorkDoneProgressCreateParams) *jsonrpc.ResponseError {
	var token string
	if err := json.Unmarshal(params.Token, &token); err != nil {
		logger.Logf("error decoding progress token: %s", err)
		return &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	ls.progressHandler.Create(token)
	return nil
}

// Close closes all the json-rpc connections.
func (ls *INOLanguageServer) Close() {
	if ls.Clangd != nil {
		ls.Clangd.Close()
		ls.Clangd = nil
	}
	if ls.closing != nil {
		close(ls.closing)
		ls.closing = nil
	}
}

// CloseNotify returns a channel that is closed when the InoHandler is closed
func (ls *INOLanguageServer) CloseNotify() <-chan bool {
	return ls.closing
}

// CleanUp performs cleanup of the workspace and temp files create by the language server
func (ls *INOLanguageServer) CleanUp() {
	if ls.buildPath != nil {
		ls.buildPath.RemoveAll()
		ls.buildPath = nil
	}
}

func (ls *INOLanguageServer) initializeWorkbench(logger jsonrpc.FunctionLogger, params *lsp.InitializeParams) error {
	// TODO: This function must be split into two
	// -> start clang (when params != nil)
	// -> reser clang status (when params == nil)
	// the two flows shares very little

	currCppTextVersion := 0
	if params != nil {
		logger.Logf("    --> initialize(%s)", params.RootURI)
		ls.sketchRoot = params.RootURI.AsPath()
		ls.sketchName = ls.sketchRoot.Base()
		ls.buildSketchCpp = ls.buildSketchRoot.Join(ls.sketchName + ".ino.cpp")

		ls.lspInitializeParams = params
		ls.lspInitializeParams.RootPath = ls.buildSketchRoot.String()
		ls.lspInitializeParams.RootURI = lsp.NewDocumentURIFromPath(ls.buildSketchRoot)
	} else {
		logger.Logf("    --> RE-initialize()")
		currCppTextVersion = ls.sketchMapper.CppText.Version
	}

	if err := ls.generateBuildEnvironment(logger); err != nil {
		return err
	}

	if cppContent, err := ls.buildSketchCpp.ReadFile(); err == nil {
		ls.sketchMapper = sourcemapper.CreateInoMapper(cppContent)
		ls.sketchMapper.CppText.Version = currCppTextVersion + 1
	} else {
		return errors.WithMessage(err, "reading generated cpp file from sketch")
	}

	if params == nil {
		// If we are restarting re-synchronize clangd
		cppURI := lsp.NewDocumentURIFromPath(ls.buildSketchCpp)

		logger.Logf("Sending 'didSave' notification to Clangd")

		didSaveParams := &lsp.DidSaveTextDocumentParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: cppURI},
			Text:         ls.sketchMapper.CppText.Text,
		}
		if err := ls.Clangd.conn.TextDocumentDidSave(didSaveParams); err != nil {
			logger.Logf("    error reinitilizing clangd:", err)
			return err
		}

		logger.Logf("Sending 'didChange' notification to Clangd")
		didChangeParams := &lsp.DidChangeTextDocumentParams{
			TextDocument: lsp.VersionedTextDocumentIdentifier{
				TextDocumentIdentifier: lsp.TextDocumentIdentifier{URI: cppURI},
				Version:                ls.sketchMapper.CppText.Version,
			},
			ContentChanges: []lsp.TextDocumentContentChangeEvent{
				{Text: ls.sketchMapper.CppText.Text},
			},
		}
		if err := ls.Clangd.conn.TextDocumentDidChange(didChangeParams); err != nil {
			logger.Logf("    error reinitilizing clangd:", err)
			return err
		}
	} else {
		// Otherwise start clangd!
		dataFolder, err := extractDataFolderFromArduinoCLI(logger)
		if err != nil {
			logger.Logf("    error: %s", err)
		}

		// Start clangd
		ls.Clangd = NewClangdLSPClient(logger, ls.buildPath, ls.buildSketchCpp, dataFolder, ls)
		go func() {
			defer streams.CatchAndLogPanic()
			ls.Clangd.Run()
			logger.Logf("Lost connection with clangd!")
			ls.Close()
		}()

		// Send initialization command to clangd
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		initRes, clangErr, err := ls.Clangd.conn.Initialize(ctx, ls.lspInitializeParams)
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

		if err := ls.Clangd.conn.Initialized(&lsp.InitializedParams{}); err != nil {
			logger.Logf("    error sending initialized notification to clangd: %v", err)
			return err
		}
	}

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

func (ls *INOLanguageServer) refreshCppDocumentSymbols(logger jsonrpc.FunctionLogger) error {
	// Query source code symbols
	ls.readUnlock(logger)
	cppURI := lsp.NewDocumentURIFromPath(ls.buildSketchCpp)
	logger.Logf("requesting documentSymbol for %s", cppURI)

	cppParams := &lsp.DocumentSymbolParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: cppURI},
	}
	cppDocumentSymbols, _ /* cppSymbolInformation */, cppErr, err := ls.Clangd.conn.TextDocumentDocumentSymbol(context.Background(), cppParams)
	ls.readLock(logger, true)
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
	ls.buildSketchSymbols = cppDocumentSymbols

	symbolsCanary := ""
	for _, symbol := range cppDocumentSymbols {
		logger.Logf("   symbol: %s %s %s", symbol.Kind, symbol.Name, symbol.Range)
		if symbolText, err := textutils.ExtractRange(ls.sketchMapper.CppText.Text, symbol.Range); err != nil {
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
	ls.buildSketchSymbolsCanary = symbolsCanary
	return nil
}

func (ls *INOLanguageServer) CheckCppIncludesChanges() {
	logger := NewLSPFunctionLogger(color.HiBlueString, "INCK --- ")
	logger.Logf("check for Cpp Include Changes")
	includesCanary := ""
	for _, line := range strings.Split(ls.sketchMapper.CppText.Text, "\n") {
		if strings.Contains(line, "#include ") {
			includesCanary += line
		}
	}

	if includesCanary != ls.buildSketchIncludesCanary {
		ls.buildSketchIncludesCanary = includesCanary
		logger.Logf("#include change detected, triggering sketch rebuild!")
		ls.scheduleRebuildEnvironment()
	}
}

func (ls *INOLanguageServer) didClose(logger jsonrpc.FunctionLogger, inoDidClose *lsp.DidCloseTextDocumentParams) (*lsp.DidCloseTextDocumentParams, error) {
	inoIdentifier := inoDidClose.TextDocument
	if _, exist := ls.trackedInoDocs[inoIdentifier.URI.AsPath().String()]; exist {
		delete(ls.trackedInoDocs, inoIdentifier.URI.AsPath().String())
	} else {
		logger.Logf("    didClose of untracked document: %s", inoIdentifier.URI)
		return nil, unknownURI(inoIdentifier.URI)
	}

	// If we are tracking a .ino...
	if inoIdentifier.URI.Ext() == ".ino" {
		ls.sketchTrackedFilesCount--
		logger.Logf("    decreasing .ino tracked files count: %d", ls.sketchTrackedFilesCount)

		// notify clang that sketch.cpp.ino has been closed only once all .ino are closed
		if ls.sketchTrackedFilesCount != 0 {
			return nil, nil
		}
	}

	cppIdentifier, err := ls.ino2cppTextDocumentIdentifier(logger, inoIdentifier)
	return &lsp.DidCloseTextDocumentParams{
		TextDocument: cppIdentifier,
	}, err
}

func (ls *INOLanguageServer) ino2cppTextDocumentItem(logger jsonrpc.FunctionLogger, inoItem lsp.TextDocumentItem) (cppItem lsp.TextDocumentItem, err error) {
	cppURI, err := ls.ino2cppDocumentURI(logger, inoItem.URI)
	if err != nil {
		return cppItem, err
	}
	cppItem.URI = cppURI

	if cppURI.AsPath().EquivalentTo(ls.buildSketchCpp) {
		cppItem.LanguageID = "cpp"
		cppItem.Text = ls.sketchMapper.CppText.Text
		cppItem.Version = ls.sketchMapper.CppText.Version
	} else {
		cppItem.LanguageID = inoItem.LanguageID
		inoPath := inoItem.URI.AsPath().String()
		cppItem.Text = ls.trackedInoDocs[inoPath].Text
		cppItem.Version = ls.trackedInoDocs[inoPath].Version
	}

	return cppItem, nil
}

func (ls *INOLanguageServer) didChange(logger jsonrpc.FunctionLogger, inoDidChangeParams *lsp.DidChangeTextDocumentParams) (*lsp.DidChangeTextDocumentParams, error) {
	inoDoc := inoDidChangeParams.TextDocument

	// Apply the change to the tracked sketch file.
	trackedInoID := inoDoc.URI.AsPath().String()
	trackedInoDoc, ok := ls.trackedInoDocs[trackedInoID]
	if !ok {
		return nil, unknownURI(inoDoc.URI)
	}
	if updatedTrackedInoDoc, err := textutils.ApplyLSPTextDocumentContentChangeEvent(trackedInoDoc, inoDidChangeParams.ContentChanges, inoDoc.Version); err != nil {
		return nil, err
	} else {
		ls.trackedInoDocs[trackedInoID] = updatedTrackedInoDoc
	}

	logger.Logf("Tracked SKETCH file:----------+\n" + ls.trackedInoDocs[trackedInoID].Text + "\n----------------------")

	// If the file is not part of a .ino flie forward the change as-is to clangd
	if inoDoc.URI.Ext() != ".ino" {
		if cppDoc, err := ls.ino2cppVersionedTextDocumentIdentifier(logger, inoDidChangeParams.TextDocument); err != nil {
			return nil, err
		} else {
			cppDidChangeParams := *inoDidChangeParams
			cppDidChangeParams.TextDocument = cppDoc
			return &cppDidChangeParams, nil
		}
	}

	// If changes are applied to a .ino file we increment the global .ino.cpp versioning
	// for each increment of the single .ino file.

	cppChanges := []lsp.TextDocumentContentChangeEvent{}
	for _, inoChange := range inoDidChangeParams.ContentChanges {
		cppChangeRange, ok := ls.sketchMapper.InoToCppLSPRangeOk(inoDoc.URI, *inoChange.Range)
		if !ok {
			return nil, errors.Errorf("invalid change range %s:%s", inoDoc.URI, inoChange.Range)
		}

		// Detect changes in critical lines (for example function definitions)
		// and trigger arduino-preprocessing + clangd restart.
		dirty := false
		for _, sym := range ls.buildSketchSymbols {
			if sym.SelectionRange.Overlaps(cppChangeRange) {
				dirty = true
				logger.Logf("--! DIRTY CHANGE detected using symbol tables, force sketch rebuild!")
				break
			}
		}
		if ls.sketchMapper.ApplyTextChange(inoDoc.URI, inoChange) {
			dirty = true
			logger.Logf("--! DIRTY CHANGE detected with sketch mapper, force sketch rebuild!")
		}
		if dirty {
			ls.scheduleRebuildEnvironment()
		}

		logger.Logf("New version:----------+\n" + ls.sketchMapper.CppText.Text + "\n----------------------")

		cppChanges = append(cppChanges, lsp.TextDocumentContentChangeEvent{
			Range:       &cppChangeRange,
			RangeLength: inoChange.RangeLength,
			Text:        inoChange.Text,
		})
	}

	ls.CheckCppIncludesChanges()

	// build a cpp equivalent didChange request
	return &lsp.DidChangeTextDocumentParams{
		ContentChanges: cppChanges,
		TextDocument: lsp.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: lsp.TextDocumentIdentifier{
				URI: lsp.NewDocumentURIFromPath(ls.buildSketchCpp),
			},
			Version: ls.sketchMapper.CppText.Version,
		},
	}, nil
}

func (ls *INOLanguageServer) ino2cppVersionedTextDocumentIdentifier(logger jsonrpc.FunctionLogger, doc lsp.VersionedTextDocumentIdentifier) (lsp.VersionedTextDocumentIdentifier, error) {
	cppURI, err := ls.ino2cppDocumentURI(logger, doc.URI)
	res := doc
	res.URI = cppURI
	return res, err
}

func (ls *INOLanguageServer) ino2cppTextDocumentIdentifier(logger jsonrpc.FunctionLogger, doc lsp.TextDocumentIdentifier) (lsp.TextDocumentIdentifier, error) {
	cppURI, err := ls.ino2cppDocumentURI(logger, doc.URI)
	res := doc
	res.URI = cppURI
	return res, err
}

func (ls *INOLanguageServer) ino2cppDocumentURI(logger jsonrpc.FunctionLogger, inoURI lsp.DocumentURI) (lsp.DocumentURI, error) {
	// Sketchbook/Sketch/Sketch.ino      -> build-path/sketch/Sketch.ino.cpp
	// Sketchbook/Sketch/AnotherTab.ino  -> build-path/sketch/Sketch.ino.cpp  (different section from above)
	// Sketchbook/Sketch/AnotherFile.cpp -> build-path/sketch/AnotherFile.cpp (1:1)
	// another/path/source.cpp           -> unchanged

	// Convert sketch path to build path
	inoPath := inoURI.AsPath()
	if inoPath.Ext() == ".ino" {
		return lsp.NewDocumentURIFromPath(ls.buildSketchCpp), nil
	}

	inside, err := inoPath.IsInsideDir(ls.sketchRoot)
	if err != nil {
		logger.Logf("    could not determine if '%s' is inside '%s'", inoPath, ls.sketchRoot)
		return lsp.NilURI, unknownURI(inoURI)
	}
	if !inside {
		logger.Logf("    '%s' not inside sketchroot '%s', passing doc identifier to as-is", ls.sketchRoot, inoPath)
		return inoURI, nil
	}

	rel, err := ls.sketchRoot.RelTo(inoPath)
	if err == nil {
		cppPath := ls.buildSketchRoot.JoinPath(rel)
		logger.Logf("    URI: '%s' -> '%s'", inoPath, cppPath)
		return lsp.NewDocumentURIFromPath(cppPath), nil
	}

	logger.Logf("    could not determine rel-path of '%s' in '%s': %s", inoPath, ls.sketchRoot, err)
	return lsp.NilURI, err
}

func (ls *INOLanguageServer) inoDocumentURIFromInoPath(logger jsonrpc.FunctionLogger, inoPath string) (lsp.DocumentURI, error) {
	if inoPath == sourcemapper.NotIno.File {
		return sourcemapper.NotInoURI, nil
	}
	doc, ok := ls.trackedInoDocs[inoPath]
	if !ok {
		logger.Logf("    !!! Unresolved .ino path: %s", inoPath)
		logger.Logf("    !!! Known doc paths are:")
		for p := range ls.trackedInoDocs {
			logger.Logf("    !!! > %s", p)
		}
		uri := lsp.NewDocumentURI(inoPath)
		return uri, unknownURI(uri)
	}
	return doc.URI, nil
}

func (ls *INOLanguageServer) cpp2inoDocumentURI(logger jsonrpc.FunctionLogger, cppURI lsp.DocumentURI, cppRange lsp.Range) (lsp.DocumentURI, lsp.Range, error) {
	// TODO: Split this function into 2
	//       - Cpp2inoSketchDocumentURI: converts sketch     (cppURI, cppRange) -> (inoURI, inoRange)
	//       - Cpp2inoDocumentURI      : converts non-sketch (cppURI)           -> (inoURI)              [range is the same]

	// Sketchbook/Sketch/Sketch.ino      <- build-path/sketch/Sketch.ino.cpp
	// Sketchbook/Sketch/AnotherTab.ino  <- build-path/sketch/Sketch.ino.cpp  (different section from above)
	// Sketchbook/Sketch/AnotherFile.cpp <- build-path/sketch/AnotherFile.cpp (1:1)
	// another/path/source.cpp           <- unchanged

	// Convert build path to sketch path
	cppPath := cppURI.AsPath()
	if cppPath.EquivalentTo(ls.buildSketchCpp) {
		inoPath, inoRange, err := ls.sketchMapper.CppToInoRangeOk(cppRange)
		if err == nil {
			if ls.sketchMapper.IsPreprocessedCppLine(cppRange.Start.Line) {
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
			ls.sketchMapper.DebugLogAll()
			return lsp.NilURI, lsp.NilRange, err
		}
		inoURI, err := ls.inoDocumentURIFromInoPath(logger, inoPath)
		return inoURI, inoRange, err
	}

	inside, err := cppPath.IsInsideDir(ls.buildSketchRoot)
	if err != nil {
		logger.Logf("    could not determine if '%s' is inside '%s'", cppPath, ls.buildSketchRoot)
		return lsp.NilURI, lsp.NilRange, err
	}
	if !inside {
		logger.Logf("    '%s' is not inside '%s'", cppPath, ls.buildSketchRoot)
		logger.Logf("    keep doc identifier to '%s' as-is", cppPath)
		return cppURI, cppRange, nil
	}

	rel, err := ls.buildSketchRoot.RelTo(cppPath)
	if err == nil {
		inoPath := ls.sketchRoot.JoinPath(rel).String()
		logger.Logf("    URI: '%s' -> '%s'", cppPath, inoPath)
		inoURI, err := ls.inoDocumentURIFromInoPath(logger, inoPath)
		logger.Logf("              as URI: '%s'", inoURI)
		return inoURI, cppRange, err
	}

	logger.Logf("    could not determine rel-path of '%s' in '%s': %s", cppPath, ls.buildSketchRoot, err)
	return lsp.NilURI, lsp.NilRange, err
}

func (ls *INOLanguageServer) ino2cppTextDocumentPositionParams(logger jsonrpc.FunctionLogger, inoParams lsp.TextDocumentPositionParams) (lsp.TextDocumentPositionParams, error) {
	inoTextDocument := inoParams.TextDocument
	inoPosition := inoParams.Position
	inoURI := inoTextDocument.URI
	prefix := fmt.Sprintf("TextDocumentIdentifier %s", inoParams)

	cppTextDocument, err := ls.ino2cppTextDocumentIdentifier(logger, inoTextDocument)
	if err != nil {
		logger.Logf("%s -> invalid text document: %s", prefix, err)
		return lsp.TextDocumentPositionParams{}, err
	}
	cppPosition := inoPosition
	if inoURI.Ext() == ".ino" {
		if cppLine, ok := ls.sketchMapper.InoToCppLineOk(inoURI, inoPosition.Line); ok {
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

func (ls *INOLanguageServer) ino2cppRange(logger jsonrpc.FunctionLogger, inoURI lsp.DocumentURI, inoRange lsp.Range) (lsp.DocumentURI, lsp.Range, error) {
	cppURI, err := ls.ino2cppDocumentURI(logger, inoURI)
	if err != nil {
		return lsp.NilURI, lsp.Range{}, err
	}
	if cppURI.AsPath().EquivalentTo(ls.buildSketchCpp) {
		cppRange := ls.sketchMapper.InoToCppLSPRange(inoURI, inoRange)
		return cppURI, cppRange, nil
	}
	return cppURI, inoRange, nil
}

func (ls *INOLanguageServer) cpp2inoLocationArray(logger jsonrpc.FunctionLogger, cppLocations []lsp.Location) ([]lsp.Location, error) {
	inoLocations := []lsp.Location{}
	for _, cppLocation := range cppLocations {
		inoLocation, err := ls.cpp2inoLocation(logger, cppLocation)
		if err != nil {
			logger.Logf("ERROR converting location %s: %s", cppLocation, err)
			return nil, err
		}
		inoLocations = append(inoLocations, inoLocation)
	}
	return inoLocations, nil
}

func (ls *INOLanguageServer) ino2cppDocumentRangeFormattingParams(logger jsonrpc.FunctionLogger, inoParams *lsp.DocumentRangeFormattingParams) (*lsp.DocumentRangeFormattingParams, error) {
	cppTextDocument, err := ls.ino2cppTextDocumentIdentifier(logger, inoParams.TextDocument)
	if err != nil {
		return nil, err
	}

	_, cppRange, err := ls.ino2cppRange(logger, inoParams.TextDocument.URI, inoParams.Range)
	return &lsp.DocumentRangeFormattingParams{
		TextDocument: cppTextDocument,
		Range:        cppRange,
		Options:      inoParams.Options,
	}, err
}

func (ls *INOLanguageServer) cpp2inoCodeAction(logger jsonrpc.FunctionLogger, codeAction lsp.CodeAction, uri lsp.DocumentURI) lsp.CodeAction {
	inoCodeAction := lsp.CodeAction{
		Title:       codeAction.Title,
		Kind:        codeAction.Kind,
		Edit:        ls.cpp2inoWorkspaceEdit(logger, codeAction.Edit),
		Diagnostics: codeAction.Diagnostics,
	}
	if codeAction.Command != nil {
		inoCommand := ls.cpp2inoCommand(logger, *codeAction.Command)
		inoCodeAction.Command = &inoCommand
	}
	if uri.Ext() == ".ino" {
		for i, diag := range inoCodeAction.Diagnostics {
			_, inoCodeAction.Diagnostics[i].Range = ls.sketchMapper.CppToInoRange(diag.Range)
		}
	}
	return inoCodeAction
}

func (ls *INOLanguageServer) cpp2inoCommand(logger jsonrpc.FunctionLogger, command lsp.Command) lsp.Command {
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
					if v.File.AsPath().EquivalentTo(ls.buildSketchCpp) {
						inoFile, inoSelection := ls.sketchMapper.CppToInoRange(v.Selection)
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

func (ls *INOLanguageServer) cpp2inoWorkspaceEdit(logger jsonrpc.FunctionLogger, cppWorkspaceEdit *lsp.WorkspaceEdit) *lsp.WorkspaceEdit {
	if cppWorkspaceEdit == nil {
		return nil
	}
	inoWorkspaceEdit := &lsp.WorkspaceEdit{
		Changes: map[lsp.DocumentURI][]lsp.TextEdit{},
	}
	for editURI, edits := range cppWorkspaceEdit.Changes {
		// if the edits are not relative to sketch file...
		if !editURI.AsPath().EquivalentTo(ls.buildSketchCpp) {
			// ...pass them through...
			inoWorkspaceEdit.Changes[editURI] = edits
			continue
		}

		// ...otherwise convert edits to the sketch.ino.cpp into multilpe .ino edits
		for _, edit := range edits {
			inoURI, inoRange, err := ls.cpp2inoDocumentURI(logger, editURI, edit.Range)
			if err != nil {
				logger.Logf("    error converting edit %s:%s: %s", editURI, edit.Range, err)
				continue
			}
			//inoFile, inoRange := ls.sketchMapper.CppToInoRange(edit.Range)
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

func (ls *INOLanguageServer) cpp2inoLocation(logger jsonrpc.FunctionLogger, cppLocation lsp.Location) (lsp.Location, error) {
	inoURI, inoRange, err := ls.cpp2inoDocumentURI(logger, cppLocation.URI, cppLocation.Range)
	return lsp.Location{
		URI:   inoURI,
		Range: inoRange,
	}, err
}

func (ls *INOLanguageServer) cpp2inoDocumentHighlight(logger jsonrpc.FunctionLogger, cppHighlight lsp.DocumentHighlight, cppURI lsp.DocumentURI) (lsp.DocumentHighlight, error) {
	_, inoRange, err := ls.cpp2inoDocumentURI(logger, cppURI, cppHighlight.Range)
	if err != nil {
		return lsp.DocumentHighlight{}, err
	}
	return lsp.DocumentHighlight{
		Kind:  cppHighlight.Kind,
		Range: inoRange,
	}, nil
}

func (ls *INOLanguageServer) cpp2inoTextEdits(logger jsonrpc.FunctionLogger, cppURI lsp.DocumentURI, cppEdits []lsp.TextEdit) (map[lsp.DocumentURI][]lsp.TextEdit, error) {
	logger.Logf("%s cpp/textEdit (%d elements)", cppURI, len(cppEdits))
	allInoEdits := map[lsp.DocumentURI][]lsp.TextEdit{}
	for _, cppEdit := range cppEdits {
		logger.Logf("        > %s -> %s", cppEdit.Range, strconv.Quote(cppEdit.NewText))
		inoURI, inoEdit, err := ls.cpp2inoTextEdit(logger, cppURI, cppEdit)
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

func (ls *INOLanguageServer) cpp2inoTextEdit(logger jsonrpc.FunctionLogger, cppURI lsp.DocumentURI, cppEdit lsp.TextEdit) (lsp.DocumentURI, lsp.TextEdit, error) {
	inoURI, inoRange, err := ls.cpp2inoDocumentURI(logger, cppURI, cppEdit.Range)
	inoEdit := cppEdit
	inoEdit.Range = inoRange
	return inoURI, inoEdit, err
}

func (ls *INOLanguageServer) cpp2inoDocumentSymbols(logger jsonrpc.FunctionLogger, cppSymbols []lsp.DocumentSymbol, inoRequestedURI lsp.DocumentURI) []lsp.DocumentSymbol {
	inoRequested := inoRequestedURI.AsPath().String()
	logger.Logf("    filtering for requested ino file: %s", inoRequested)
	if inoRequestedURI.Ext() != ".ino" || len(cppSymbols) == 0 {
		return cppSymbols
	}

	inoSymbols := []lsp.DocumentSymbol{}
	for _, symbol := range cppSymbols {
		logger.Logf("    > convert %s %s", symbol.Kind, symbol.Range)
		if ls.sketchMapper.IsPreprocessedCppLine(symbol.Range.Start.Line) {
			logger.Logf("      symbol is in the preprocessed section of the sketch.ino.cpp")
			continue
		}

		inoFile, inoRange := ls.sketchMapper.CppToInoRange(symbol.Range)
		inoSelectionURI, inoSelectionRange := ls.sketchMapper.CppToInoRange(symbol.SelectionRange)

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
			Children:       ls.cpp2inoDocumentSymbols(logger, symbol.Children, inoRequestedURI),
		})
	}

	return inoSymbols
}

func (ls *INOLanguageServer) cpp2inoSymbolInformation(syms []lsp.SymbolInformation) []lsp.SymbolInformation {
	panic("not implemented")
}

func (ls *INOLanguageServer) cpp2inoDiagnostics(logger jsonrpc.FunctionLogger, cppDiagsParams *lsp.PublishDiagnosticsParams) ([]*lsp.PublishDiagnosticsParams, error) {

	cppURI := cppDiagsParams.URI
	isSketch := cppURI.AsPath().EquivalentTo(ls.buildSketchCpp)

	if !isSketch {
		inoURI, _, err := ls.cpp2inoDocumentURI(logger, cppURI, lsp.NilRange)
		if err != nil {
			return nil, err
		}
		inoDiags := []lsp.Diagnostic{}
		for _, cppDiag := range cppDiagsParams.Diagnostics {
			inoURIofConvertedRange, inoRange, err := ls.cpp2inoDocumentURI(logger, cppURI, cppDiag.Range)
			if err != nil {
				return nil, err
			}
			if inoURIofConvertedRange.String() == sourcemapper.NotInoURI.String() {
				continue
			}
			if inoURIofConvertedRange.String() != inoURI.String() {
				return nil, fmt.Errorf("unexpected inoURI %s: it should be %s", inoURIofConvertedRange, inoURI)
			}
			inoDiag := cppDiag
			inoDiag.Range = inoRange
			inoDiags = append(inoDiags, inoDiag)
		}
		return []*lsp.PublishDiagnosticsParams{
			{
				URI:         inoURI,
				Diagnostics: inoDiags,
			},
		}, nil
	}

	allInoDiagsParams := map[lsp.DocumentURI]*lsp.PublishDiagnosticsParams{}
	for inoURI := range ls.inoDocsWithDiagnostics {
		allInoDiagsParams[inoURI] = &lsp.PublishDiagnosticsParams{
			URI:         inoURI,
			Diagnostics: []lsp.Diagnostic{},
		}
	}
	ls.inoDocsWithDiagnostics = map[lsp.DocumentURI]bool{}

	for _, cppDiag := range cppDiagsParams.Diagnostics {
		inoURI, inoRange, err := ls.cpp2inoDocumentURI(logger, cppURI, cppDiag.Range)
		if err != nil {
			return nil, err
		}
		if inoURI.String() == sourcemapper.NotInoURI.String() {
			continue
		}

		inoDiagsParams, ok := allInoDiagsParams[inoURI]
		if !ok {
			inoDiagsParams = &lsp.PublishDiagnosticsParams{
				URI:         inoURI,
				Diagnostics: []lsp.Diagnostic{},
			}
			allInoDiagsParams[inoURI] = inoDiagsParams
		}

		inoDiag := cppDiag
		inoDiag.Range = inoRange
		inoDiagsParams.Diagnostics = append(inoDiagsParams.Diagnostics, inoDiag)

		ls.inoDocsWithDiagnostics[inoURI] = true

		// If we have an "undefined reference" in the .ino code trigger a
		// check for newly created symbols (that in turn may trigger a
		// new arduino-preprocessing of the sketch).
		var inoDiagCode string
		if err := json.Unmarshal(inoDiag.Code, &inoDiagCode); err != nil {
			if inoDiagCode == "undeclared_var_use_suggest" ||
				inoDiagCode == "undeclared_var_use" ||
				inoDiagCode == "ovl_no_viable_function_in_call" ||
				inoDiagCode == "pp_file_not_found" {
				ls.queueCheckCppDocumentSymbols()
			}
		}
	}

	inoDiagParams := []*lsp.PublishDiagnosticsParams{}
	for _, v := range allInoDiagsParams {
		inoDiagParams = append(inoDiagParams, v)
	}
	return inoDiagParams, nil
}

func (ls *INOLanguageServer) createClangdFormatterConfig(logger jsonrpc.FunctionLogger, cppuri lsp.DocumentURI) (func(), error) {
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

	if sketchFormatterConf := ls.sketchRoot.Join(".clang-format"); sketchFormatterConf.Exist() {
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

func unknownURI(uri lsp.DocumentURI) error {
	return errors.New("Document is not available: " + uri.String())
}
