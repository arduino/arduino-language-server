package ls

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arduino/arduino-cli/executils"
	rpc "github.com/arduino/arduino-cli/rpc/cc/arduino/cli/settings/v1"
	"github.com/arduino/arduino-language-server/sourcemapper"
	"github.com/arduino/arduino-language-server/streams"
	"github.com/arduino/go-paths-helper"
	"github.com/fatih/color"
	"github.com/pkg/errors"
	"go.bug.st/json"
	"go.bug.st/lsp"
	"go.bug.st/lsp/jsonrpc"
	"go.bug.st/lsp/textedits"
	"google.golang.org/grpc"
)

// INOLanguageServer is a JSON-RPC handler that delegates messages to clangd.
type INOLanguageServer struct {
	config *Config
	IDE    *IDELSPServer
	Clangd *ClangdLSPClient

	progressHandler           *ProgressProxyHandler
	closing                   chan bool
	clangdStarted             *sync.Cond
	dataMux                   sync.RWMutex
	compileCommandsDir        *paths.Path
	buildPath                 *paths.Path
	buildSketchRoot           *paths.Path
	buildSketchCpp            *paths.Path
	sketchRoot                *paths.Path
	sketchName                string
	sketchMapper              *sourcemapper.SketchMapper
	sketchTrackedFilesCount   int
	trackedIDEDocs            map[string]lsp.TextDocumentItem
	ideInoDocsWithDiagnostics map[lsp.DocumentURI]bool
	sketchRebuilder           *SketchRebuilder
}

// Config describes the language server configuration.
type Config struct {
	Fqbn              string
	CliPath           *paths.Path
	CliConfigPath     *paths.Path
	ClangdPath        *paths.Path
	CliDaemonAddress  string
	CliInstanceNumber int
	FormatterConf     *paths.Path
	EnableLogging     bool
}

var yellow = color.New(color.FgHiYellow)

func (ls *INOLanguageServer) writeLock(logger jsonrpc.FunctionLogger, requireClangd bool) {
	ls.dataMux.Lock()
	logger.Logf(yellow.Sprintf("write-locked"))
	if requireClangd && ls.Clangd == nil {
		// if clangd is not started...
		logger.Logf("(throttled: waiting for clangd)")
		logger.Logf(yellow.Sprintf("unlocked (waiting clangd)"))
		ls.clangdStarted.Wait()
		logger.Logf(yellow.Sprintf("locked (waiting clangd)"))

		if ls.Clangd == nil {
			logger.Logf("clangd startup failed: quitting Language server")
			ls.Close()
			os.Exit(2)
		}
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

// NewINOLanguageServer creates and configures an Arduino Language Server.
func NewINOLanguageServer(stdin io.Reader, stdout io.Writer, config *Config) *INOLanguageServer {
	logger := NewLSPFunctionLogger(color.HiWhiteString, "LS: ")
	ls := &INOLanguageServer{
		trackedIDEDocs:            map[string]lsp.TextDocumentItem{},
		ideInoDocsWithDiagnostics: map[lsp.DocumentURI]bool{},
		closing:                   make(chan bool),
		config:                    config,
	}
	ls.clangdStarted = sync.NewCond(&ls.dataMux)
	ls.sketchRebuilder = NewSketchBuilder(ls)

	if tmp, err := paths.MkTempDir("", "arduino-language-server"); err != nil {
		log.Fatalf("Could not create temp folder: %s", err)
	} else {
		ls.compileCommandsDir = tmp.Canonical()
	}

	if tmp, err := paths.MkTempDir("", "arduino-language-server"); err != nil {
		log.Fatalf("Could not create temp folder: %s", err)
	} else {
		ls.buildPath = tmp.Canonical()
		ls.buildSketchRoot = ls.buildPath.Join("sketch")
	}

	logger.Logf("Initial board configuration: %s", ls.config.Fqbn)
	logger.Logf("Language server build path: %s", ls.buildPath)
	logger.Logf("Language server build sketch root: %s", ls.buildSketchRoot)
	logger.Logf("Language server compile-commands: %s", ls.compileCommandsDir.Join("compile_commands.json"))

	ls.IDE = NewIDELSPServer(logger, stdin, stdout, ls)
	ls.progressHandler = NewProgressProxy(ls.IDE.conn)
	go func() {
		defer streams.CatchAndLogPanic()
		ls.IDE.Run()
		logger.Logf("Lost connection with IDE!")
		ls.Close()
	}()

	return ls
}

func (ls *INOLanguageServer) InitializeReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.InitializeParams) (*lsp.InitializeResult, *jsonrpc.ResponseError) {
	go func() {
		defer streams.CatchAndLogPanic()
		// Unlock goroutines waiting for clangd
		defer ls.clangdStarted.Broadcast()

		logger := NewLSPFunctionLogger(color.HiCyanString, "INIT --- ")
		logger.Logf("initializing workbench: %s", inoParams.RootURI)

		ls.sketchRoot = inoParams.RootURI.AsPath()
		ls.sketchName = ls.sketchRoot.Base()
		ls.buildSketchCpp = ls.buildSketchRoot.Join(ls.sketchName + ".ino.cpp")

		if success, err := ls.generateBuildEnvironment(context.Background(), logger); err != nil {
			logger.Logf("error starting clang: %s", err)
			return
		} else if !success {
			logger.Logf("bootstrap build failed!")
			return
		}

		if err := ls.buildPath.Join("compile_commands.json").CopyTo(ls.compileCommandsDir.Join("compile_commands.json")); err != nil {
			logger.Logf("ERROR: updating compile_commands: %s", err)
		}

		if cppContent, err := ls.buildSketchCpp.ReadFile(); err == nil {
			ls.sketchMapper = sourcemapper.CreateInoMapper(cppContent)
			ls.sketchMapper.CppText.Version = 1
		} else {
			logger.Logf("error starting clang: reading generated cpp file from sketch: %s", err)
			return
		}

		// Retrieve data folder
		dataFolder, err := ls.extractDataFolderFromArduinoCLI(logger)
		if err != nil {
			logger.Logf("error retrieving data folder from arduino-cli: %s", err)
			return
		}

		// Start clangd
		ls.Clangd = NewClangdLSPClient(logger, dataFolder, ls)
		go func() {
			defer streams.CatchAndLogPanic()
			ls.Clangd.Run()
			logger.Logf("Lost connection with clangd!")
			ls.Close()
		}()

		// Send initialization command to clangd (1 sec. timeout)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		cppInitializeParams := *inoParams
		cppInitializeParams.RootPath = ls.buildSketchRoot.String()
		cppInitializeParams.RootURI = lsp.NewDocumentURIFromPath(ls.buildSketchRoot)
		if initRes, clangErr, err := ls.Clangd.conn.Initialize(ctx, &cppInitializeParams); err != nil {
			logger.Logf("error initilizing clangd: %v", err)
			return
		} else if clangErr != nil {
			logger.Logf("error initilizing clangd: %v", clangErr.AsError())
			return
		} else {
			logger.Logf("clangd successfully started: %s", string(lsp.EncodeMessage(initRes)))
		}

		if err := ls.Clangd.conn.Initialized(&lsp.InitializedParams{}); err != nil {
			logger.Logf("error sending initialized notification to clangd: %v", err)
			return
		}

		logger.Logf("Done initializing workbench")
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
	ls.Clangd.conn.Shutdown(context.Background())
	return nil
}

func (ls *INOLanguageServer) TextDocumentCompletionReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.CompletionParams) (*lsp.CompletionList, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	logger.Logf("--> completion(%s)\n", inoParams.TextDocument)
	cppTextDocPositionParams, err := ls.ide2ClangTextDocumentPositionParams(logger, inoParams.TextDocumentPositionParams)
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

func (ls *INOLanguageServer) TextDocumentHoverReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, ideParams *lsp.HoverParams) (*lsp.Hover, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	clangTextDocPosition, err := ls.ide2ClangTextDocumentPositionParams(logger, ideParams.TextDocumentPositionParams)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	clangParams := &lsp.HoverParams{
		TextDocumentPositionParams: clangTextDocPosition,
	}
	clangResp, clangErr, err := ls.Clangd.conn.TextDocumentHover(ctx, clangParams)
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
		logger.Logf("response: nil")
		return nil, nil
	}

	_, r, err := ls.clang2IdeRangeAndDocumentURI(logger, clangParams.TextDocument.URI, *clangResp.Range)
	if err != nil {
		logger.Logf("error during range conversion: %v", err)
		ls.Close()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	ideResp := lsp.Hover{
		Contents: clangResp.Contents,
		Range:    &r,
	}
	logger.Logf("Hover content: %s", strconv.Quote(ideResp.Contents.Value))
	return &ideResp, nil
}

func (ls *INOLanguageServer) clangURIRefersToIno(uri lsp.DocumentURI) bool {
	return uri.AsPath().EquivalentTo(ls.buildSketchCpp)
}

func (ls *INOLanguageServer) TextDocumentSignatureHelpReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.SignatureHelpParams) (*lsp.SignatureHelp, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	inoTextDocumentPosition := inoParams.TextDocumentPositionParams

	logger.Logf("%s", inoTextDocumentPosition)
	cppTextDocumentPosition, err := ls.ide2ClangTextDocumentPositionParams(logger, inoTextDocumentPosition)
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
	cppTextDocPosition, err := ls.ide2ClangTextDocumentPositionParams(logger, inoTextDocPosition)
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
	cppTextDocumentPosition, err := ls.ide2ClangTextDocumentPositionParams(logger, inoTextDocumentPosition)
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

	cppTextDocumentPosition, err := ls.ide2ClangTextDocumentPositionParams(logger, inoTextDocumentPosition)
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

func (ls *INOLanguageServer) TextDocumentDocumentHighlightReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, ideParams *lsp.DocumentHighlightParams) ([]lsp.DocumentHighlight, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	clangTextDocumentPosition, err := ls.ide2ClangTextDocumentPositionParams(logger, ideParams.TextDocumentPositionParams)
	if err != nil {
		logger.Logf("ERROR: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	clangURI := clangTextDocumentPosition.TextDocument.URI

	clangParams := *ideParams
	clangParams.TextDocumentPositionParams = clangTextDocumentPosition
	clangHighlights, clangErr, err := ls.Clangd.conn.TextDocumentDocumentHighlight(ctx, &clangParams)
	if err != nil {
		logger.Logf("clangd connectiono ERROR: %v", err)
		ls.Close()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response ERROR: %v", clangErr.AsError())
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	if clangHighlights == nil {
		logger.Logf("null response from clangd")
		return nil, nil
	}

	ideHighlights := []lsp.DocumentHighlight{}
	for _, clangHighlight := range clangHighlights {
		ideHighlight, err := ls.clang2IdeDocumentHighlight(logger, clangHighlight, clangURI)
		if err != nil {
			logger.Logf("ERROR converting highlight %s:%s: %s", clangURI, clangHighlight.Range, err)
			return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
		}
		ideHighlights = append(ideHighlights, ideHighlight)
	}
	return ideHighlights, nil
}

func (ls *INOLanguageServer) TextDocumentDocumentSymbolReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, inoParams *lsp.DocumentSymbolParams) ([]lsp.DocumentSymbol, []lsp.SymbolInformation, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	inoTextDocument := inoParams.TextDocument
	inoURI := inoTextDocument.URI
	logger.Logf("--> %s")

	cppTextDocument, err := ls.ide2ClangTextDocumentIdentifier(logger, inoTextDocument)
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
		inoSymbolInformation = ls.clang2IdeSymbolInformation(cppSymbolInformation)
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
	cppTextDocument, err := ls.ide2ClangTextDocumentIdentifier(logger, inoTextDocument)
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

	cppTextDocument, err := ls.ide2ClangTextDocumentIdentifier(logger, inoTextDocument)
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
	cppParams, err := ls.ide2ClangDocumentRangeFormattingParams(logger, inoParams)
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
	ls.Clangd.conn.Exit()
	logger.Logf("Arduino Language Server is shutting down.")
	os.Exit(0)
}

func (ls *INOLanguageServer) TextDocumentDidOpenNotifFromIDE(logger jsonrpc.FunctionLogger, inoParam *lsp.DidOpenTextDocumentParams) {
	ls.writeLock(logger, true)
	defer ls.writeUnlock(logger)

	ls.triggerRebuild()

	// Add the TextDocumentItem in the tracked files list
	inoTextDocItem := inoParam.TextDocument
	ls.trackedIDEDocs[inoTextDocItem.URI.AsPath().String()] = inoTextDocItem

	// If we are tracking a .ino...
	if inoTextDocItem.URI.Ext() == ".ino" {
		ls.sketchTrackedFilesCount++
		logger.Logf("Increasing .ino tracked files count to %d", ls.sketchTrackedFilesCount)

		// Notify clangd that sketchCpp has been opened only once
		if ls.sketchTrackedFilesCount != 1 {
			logger.Logf("Clang already notified, do not notify it anymore")
			return
		}
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

	ls.triggerRebuild()

	logger.Logf("didChange(%s)", inoParams.TextDocument)
	for _, change := range inoParams.ContentChanges {
		logger.Logf("  > %s", change)
	}

	if cppParams, err := ls.didChange(logger, inoParams); err != nil {
		logger.Logf("Error: %s", err)
	} else if cppParams == nil {
		logger.Logf("Notification is not propagated to clangd")
	} else {
		logger.Logf("to Clang: didChange(%s@%d)", cppParams.TextDocument)
		for _, change := range cppParams.ContentChanges {
			logger.Logf("            > %s", change)
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

	ls.triggerRebuild()

	logger.Logf("didSave(%s) hasText=%v", inoParams.TextDocument, inoParams.Text != "")
	if cppTextDocument, err := ls.ide2ClangTextDocumentIdentifier(logger, inoParams.TextDocument); err != nil {
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

	ls.triggerRebuild()

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
	ls.readLock(logger, false)
	defer ls.readUnlock(logger)

	logger.Logf("%s (%d diagnostics):", cppParams.URI, len(cppParams.Diagnostics))
	for _, diag := range cppParams.Diagnostics {
		logger.Logf("  > %s - %s: %s", diag.Range.Start, diag.Severity, string(diag.Code))
	}

	// the diagnostics on sketch.cpp.ino once mapped into their
	// .ino counter parts may span over multiple .ino files...
	allInoParams, err := ls.clang2IdeDiagnostics(logger, cppParams)
	if err != nil {
		logger.Logf("Error converting diagnostics to .ino: %s", err)
		return
	}

	// Push back to IDE the converted diagnostics
	logger.Logf("diagnostics to IDE:")
	for _, inoParams := range allInoParams {
		logger.Logf("  - %s (%d diagnostics):", inoParams.URI, len(inoParams.Diagnostics))
		for _, diag := range inoParams.Diagnostics {
			logger.Logf("    > %s - %s: %s", diag.Range.Start, diag.Severity, diag.Code)
		}
		if err := ls.IDE.conn.TextDocumentPublishDiagnostics(inoParams); err != nil {
			logger.Logf("Error sending diagnostics to IDE: %s", err)
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

// Close closes all the json-rpc connections and clean-up temp folders.
func (ls *INOLanguageServer) Close() {
	if ls.Clangd != nil {
		ls.Clangd.Close()
		ls.Clangd = nil
	}
	if ls.closing != nil {
		close(ls.closing)
		ls.closing = nil
	}
	if ls.buildPath != nil {
		ls.buildPath.RemoveAll()
	}
	if ls.compileCommandsDir != nil {
		ls.compileCommandsDir.RemoveAll()
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

func (ls *INOLanguageServer) extractDataFolderFromArduinoCLI(logger jsonrpc.FunctionLogger) (*paths.Path, error) {
	if ls.config.CliPath == nil {
		// Establish a connection with the arduino-cli gRPC server
		conn, err := grpc.Dial(ls.config.CliDaemonAddress, grpc.WithInsecure(), grpc.WithBlock())
		if err != nil {
			return nil, fmt.Errorf("error connecting to arduino-cli rpc server: %w", err)
		}
		defer conn.Close()
		client := rpc.NewSettingsServiceClient(conn)

		resp, err := client.GetValue(context.Background(), &rpc.GetValueRequest{
			Key: "directories.data",
		})
		if err != nil {
			return nil, fmt.Errorf("error getting arduino data dir: %w", err)
		}
		var dataDir string
		if err := json.Unmarshal([]byte(resp.JsonData), &dataDir); err != nil {
			return nil, fmt.Errorf("error getting arduino data dir: %w", err)
		}
		logger.Logf("Arduino Data Dir -> %s", dataDir)
		return paths.New(dataDir), nil
	} else {
		args := []string{ls.config.CliPath.String(),
			"--config-file", ls.config.CliConfigPath.String(),
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
}

func (ls *INOLanguageServer) didClose(logger jsonrpc.FunctionLogger, inoDidClose *lsp.DidCloseTextDocumentParams) (*lsp.DidCloseTextDocumentParams, error) {
	inoIdentifier := inoDidClose.TextDocument
	if _, exist := ls.trackedIDEDocs[inoIdentifier.URI.AsPath().String()]; exist {
		delete(ls.trackedIDEDocs, inoIdentifier.URI.AsPath().String())
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

	cppIdentifier, err := ls.ide2ClangTextDocumentIdentifier(logger, inoIdentifier)
	return &lsp.DidCloseTextDocumentParams{
		TextDocument: cppIdentifier,
	}, err
}

func (ls *INOLanguageServer) ino2cppTextDocumentItem(logger jsonrpc.FunctionLogger, inoItem lsp.TextDocumentItem) (cppItem lsp.TextDocumentItem, err error) {
	cppURI, err := ls.ide2ClangDocumentURI(logger, inoItem.URI)
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
		cppItem.Text = ls.trackedIDEDocs[inoPath].Text
		cppItem.Version = ls.trackedIDEDocs[inoPath].Version
	}

	return cppItem, nil
}

func (ls *INOLanguageServer) didChange(logger jsonrpc.FunctionLogger, inoDidChangeParams *lsp.DidChangeTextDocumentParams) (*lsp.DidChangeTextDocumentParams, error) {
	// Clear all RangeLengths: it's a deprecated field and if the byte count is wrong the
	// source text file will be unloaded from clangd without notice, leading to a "non-added
	// document" error for all subsequent requests.
	// https://github.com/clangd/clangd/issues/717#issuecomment-793220007
	for i := range inoDidChangeParams.ContentChanges {
		inoDidChangeParams.ContentChanges[i].RangeLength = nil
	}

	inoDoc := inoDidChangeParams.TextDocument

	// Apply the change to the tracked sketch file.
	trackedInoID := inoDoc.URI.AsPath().String()
	if doc, ok := ls.trackedIDEDocs[trackedInoID]; !ok {
		return nil, unknownURI(inoDoc.URI)
	} else if updatedDoc, err := textedits.ApplyLSPTextDocumentContentChangeEvent(doc, inoDidChangeParams); err != nil {
		return nil, err
	} else {
		ls.trackedIDEDocs[trackedInoID] = updatedDoc
	}

	logger.Logf("Tracked SKETCH file:----------+\n" + ls.trackedIDEDocs[trackedInoID].Text + "\n----------------------")

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

		_ = ls.sketchMapper.ApplyTextChange(inoDoc.URI, inoChange)

		ls.sketchMapper.DebugLogAll()

		cppChanges = append(cppChanges, lsp.TextDocumentContentChangeEvent{
			Range:       &cppChangeRange,
			RangeLength: inoChange.RangeLength,
			Text:        inoChange.Text,
		})
	}

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
	cppURI, err := ls.ide2ClangDocumentURI(logger, doc.URI)
	res := doc
	res.URI = cppURI
	return res, err
}

func (ls *INOLanguageServer) ide2ClangRange(logger jsonrpc.FunctionLogger, ideURI lsp.DocumentURI, ideRange lsp.Range) (lsp.DocumentURI, lsp.Range, error) {
	clangURI, err := ls.ide2ClangDocumentURI(logger, ideURI)
	if err != nil {
		return lsp.DocumentURI{}, lsp.Range{}, err
	}
	if clangURI.AsPath().EquivalentTo(ls.buildSketchCpp) {
		cppRange := ls.sketchMapper.InoToCppLSPRange(ideURI, ideRange)
		return clangURI, cppRange, nil
	}
	return clangURI, ideRange, nil
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

func (ls *INOLanguageServer) ide2ClangDocumentRangeFormattingParams(logger jsonrpc.FunctionLogger, ideParams *lsp.DocumentRangeFormattingParams) (*lsp.DocumentRangeFormattingParams, error) {
	clangTextDocumentIdentifier, err := ls.ide2ClangTextDocumentIdentifier(logger, ideParams.TextDocument)
	if err != nil {
		return nil, err
	}

	_, clangRange, err := ls.ide2ClangRange(logger, ideParams.TextDocument.URI, ideParams.Range)
	return &lsp.DocumentRangeFormattingParams{
		WorkDoneProgressParams: ideParams.WorkDoneProgressParams,
		Options:                ideParams.Options,
		TextDocument:           clangTextDocumentIdentifier,
		Range:                  clangRange,
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
			inoURI, inoRange, err := ls.clang2IdeRangeAndDocumentURI(logger, editURI, edit.Range)
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
	inoURI, inoRange, err := ls.clang2IdeRangeAndDocumentURI(logger, cppLocation.URI, cppLocation.Range)
	return lsp.Location{
		URI:   inoURI,
		Range: inoRange,
	}, err
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
	inoURI, inoRange, err := ls.clang2IdeRangeAndDocumentURI(logger, cppURI, cppEdit.Range)
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

func unknownURI(uri lsp.DocumentURI) error {
	return errors.New("Document is not available: " + uri.String())
}
