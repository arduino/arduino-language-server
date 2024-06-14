// This file is part of arduino-language-server.
//
// Copyright 2022 ARDUINO SA (http://www.arduino.cc/)
//
// This software is released under the GNU Affero General Public License version 3,
// which covers the main part of arduino-language-server.
// The terms of this license can be found at:
// https://www.gnu.org/licenses/agpl-3.0.html
//
// You can be released from the requirements of the above licenses by purchasing
// a commercial license. Buying such a license is mandatory if you want to
// modify or otherwise use the software for commercial activities involving the
// Arduino software without disclosing the source code of your own applications.
// To purchase a commercial license, send an email to license@arduino.cc.

package ls

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	rpc "github.com/arduino/arduino-cli/rpc/cc/arduino/cli/commands/v1"
	"github.com/arduino/arduino-language-server/globals"
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
	"google.golang.org/grpc/credentials/insecure"
)

// INOLanguageServer is a JSON-RPC handler that delegates messages to clangd.
type INOLanguageServer struct {
	config *Config
	IDE    *IDELSPServer
	Clangd *clangdLSPClient

	progressHandler           *progressProxyHandler
	closing                   chan bool
	removeTempMutex           sync.Mutex
	clangdStarted             *sync.Cond
	dataMux                   sync.RWMutex
	tempDir                   *paths.Path
	buildPath                 *paths.Path
	buildSketchRoot           *paths.Path
	buildSketchCpp            *paths.Path
	fullBuildPath             *paths.Path
	sketchRoot                *paths.Path
	sketchName                string
	sketchMapper              *sourcemapper.SketchMapper
	sketchTrackedFilesCount   int
	trackedIdeDocs            map[string]lsp.TextDocumentItem
	ideInoDocsWithDiagnostics map[lsp.DocumentURI]bool
	sketchRebuilder           *sketchRebuilder
}

// Config describes the language server configuration.
type Config struct {
	Fqbn                            string
	CliPath                         *paths.Path
	CliConfigPath                   *paths.Path
	ClangdPath                      *paths.Path
	CliDaemonAddress                string
	CliInstanceNumber               int
	FormatterConf                   *paths.Path
	EnableLogging                   bool
	SkipLibrariesDiscoveryOnRebuild bool
	DisableRealTimeDiagnostics      bool
	Jobs                            int
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
		trackedIdeDocs:            map[string]lsp.TextDocumentItem{},
		ideInoDocsWithDiagnostics: map[lsp.DocumentURI]bool{},
		closing:                   make(chan bool),
		config:                    config,
	}
	ls.clangdStarted = sync.NewCond(&ls.dataMux)
	ls.sketchRebuilder = newSketchBuilder(ls)

	if tmp, err := paths.MkTempDir("", "arduino-language-server"); err != nil {
		log.Fatalf("Could not create temp folder: %s", err)
	} else {
		ls.tempDir = tmp.Canonical()
	}
	ls.buildPath = ls.tempDir.Join("build")
	ls.buildSketchRoot = ls.buildPath.Join("sketch")
	if err := ls.buildPath.MkdirAll(); err != nil {
		log.Fatalf("Could not create temp folder: %s", err)
	}
	ls.fullBuildPath = ls.tempDir.Join("fullbuild")
	if err := ls.fullBuildPath.MkdirAll(); err != nil {
		log.Fatalf("Could not create temp folder: %s", err)
	}

	logger.Logf("Initial board configuration: %s", ls.config.Fqbn)
	logger.Logf("%s", globals.VersionInfo.String())
	logger.Logf("Language server temp directory: %s", ls.tempDir)
	logger.Logf("Language server build path: %s", ls.buildPath)
	logger.Logf("Language server build sketch root: %s", ls.buildSketchRoot)
	logger.Logf("Language server FULL build path: %s", ls.fullBuildPath)

	ls.IDE = NewIDELSPServer(logger, stdin, stdout, ls)
	ls.progressHandler = newProgressProxy(ls.IDE.conn)
	go func() {
		defer streams.CatchAndLogPanic()
		ls.IDE.Run()
		logger.Logf("Lost connection with IDE!")
		ls.Close()
	}()

	return ls
}

func (ls *INOLanguageServer) initializeReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, ideParams *lsp.InitializeParams) (*lsp.InitializeResult, *jsonrpc.ResponseError) {
	ls.writeLock(logger, false)
	ls.sketchRoot = ideParams.RootURI.AsPath()
	ls.sketchName = ls.sketchRoot.Base()
	ls.buildSketchCpp = ls.buildSketchRoot.Join(ls.sketchName + ".ino.cpp")
	ls.writeUnlock(logger)

	go func() {
		defer streams.CatchAndLogPanic()

		// Unlock goroutines waiting for clangd at the end of the initialization.
		defer ls.clangdStarted.Broadcast()

		logger := NewLSPFunctionLogger(color.HiCyanString, "INIT --- ")
		logger.Logf("initializing workbench: %s", ideParams.RootURI)

		if success, err := ls.generateBuildEnvironment(context.Background(), true, logger); err != nil {
			logger.Logf("error starting clang: %s", err)
			return
		} else if !success {
			logger.Logf("bootstrap build failed!")
			return
		}

		if inoCppContent, err := ls.buildSketchCpp.ReadFile(); err == nil {
			ls.sketchMapper = sourcemapper.CreateInoMapper(inoCppContent)
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
		ls.Clangd = newClangdLSPClient(logger, dataFolder, ls)
		go func() {
			defer streams.CatchAndLogPanic()
			ls.Clangd.Run()
			logger.Logf("Lost connection with clangd!")
			ls.Close()
		}()

		// Send initialization command to clangd (1 sec. timeout)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		clangInitializeParams := *ideParams
		clangInitializeParams.RootPath = ls.buildSketchRoot.String()
		clangInitializeParams.RootURI = lsp.NewDocumentURIFromPath(ls.buildSketchRoot)
		if clangInitializeResult, clangErr, err := ls.Clangd.conn.Initialize(ctx, &clangInitializeParams); err != nil {
			logger.Logf("error initializing clangd: %v", err)
			return
		} else if clangErr != nil {
			logger.Logf("error initializing clangd: %v", clangErr.AsError())
			return
		} else {
			logger.Logf("clangd successfully started: %s", string(lsp.EncodeMessage(clangInitializeResult)))
		}

		if err := ls.Clangd.conn.Initialized(&lsp.InitializedParams{}); err != nil {
			logger.Logf("error sending initialized notification to clangd: %v", err)
			return
		}

		logger.Logf("Done initializing workbench")
	}()
	/*
		Clang 12 capabilities:

		✓	"textDocumentSync": {
		✓		"openClose": true,
		✓		"change": 2, (incremental)
		✓		"save": {}
		✓	},
		✓	"completionProvider": {
		✓		"triggerCharacters": [ ".", "<", ">", ":", "\"", "/" ],
		✓		"allCommitCharacters": [
		✓			" ", "\t","(", ")", "[", "]", "{", "}", "<",
		✓			">", ":", ";", ",", "+", "-", "/", "*", "%",
		✓			"^", "&", "#", "?", ".", "=", "\"","'",	"|"
		✓		],
		✓		"completionItem": {}
		✓	},
		✓	"hoverProvider": {},
		✓	"signatureHelpProvider": {
		✓		"triggerCharacters": [ "(", ","	]
		✓	},
		✓	"declarationProvider": {},
		✓	"definitionProvider": {},
		✓	"implementationProvider": {},
		✓	"referencesProvider": {},
		✓	"documentHighlightProvider": {},
		✓	"documentSymbolProvider": {},
		✓	"codeActionProvider": {
		✓		"codeActionKinds": [ "quickfix", "refactor", "info"	]
		✓	},
		✓	"documentLinkProvider": {},
		✓	"documentFormattingProvider": {},
		✓	"documentRangeFormattingProvider": {},
		✓	"documentOnTypeFormattingProvider": {
		✓		"firstTriggerCharacter": "\n"
		✓	},
		✓	"renameProvider": {
		✓		"prepareProvider": true
		✓	},
		✓	"executeCommandProvider": {
		✓		"commands": [ "clangd.applyFix", "clangd.applyTweak" ]
		✓	},
		✓	"selectionRangeProvider": {},
		✓	"callHierarchyProvider": {},
		✓	"semanticTokensProvider": {
		✓		"legend": {
		✓			"tokenTypes": [
		✓				"variable",	"variable", "parameter", "function", "method",
		✓				"function", "property", "variable", "class", "enum",
		✓				"enumMember", "type", "dependent", "dependent", "namespace",
		✓				"typeParameter", "concept", "type", "macro", "comment"
		✓			],
		✓			"tokenModifiers": []
		✓		},
		✓		"range": false,
		✓		"full": {
		✓			"delta": true
		✓		}
		✓	},
		✓	"workspaceSymbolProvider": {}
	*/
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
				TriggerCharacters: []string{".", "<", ">", ":", "\"", "/"},
				AllCommitCharacters: []string{
					" ", "\t", "(", ")", "[", "]", "{", "}", "<", ">",
					":", ";", ",", "+", "-", "/", "*", "%", "^", "&",
					"#", "?", ".", "=", "\"", "'", "|"},
				ResolveProvider: false,
				CompletionItem:  &lsp.CompletionItemOptions{},
			},
			HoverProvider: &lsp.HoverOptions{},
			SignatureHelpProvider: &lsp.SignatureHelpOptions{
				TriggerCharacters: []string{"(", ","},
			},
			// DeclarationProvider:             &lsp.DeclarationRegistrationOptions{},
			DefinitionProvider: &lsp.DefinitionOptions{},
			// ImplementationProvider:          &lsp.ImplementationRegistrationOptions{},
			// ReferencesProvider:              &lsp.ReferenceOptions{},
			DocumentHighlightProvider: &lsp.DocumentHighlightOptions{},
			DocumentSymbolProvider:    &lsp.DocumentSymbolOptions{},
			CodeActionProvider: &lsp.CodeActionOptions{
				CodeActionKinds: []lsp.CodeActionKind{
					lsp.CodeActionKindQuickFix,
					lsp.CodeActionKindRefactor,
					"info",
				},
			},
			// DocumentLinkProvider:            &lsp.DocumentLinkOptions{ResolveProvider: false},
			DocumentFormattingProvider:      &lsp.DocumentFormattingOptions{},
			DocumentRangeFormattingProvider: &lsp.DocumentRangeFormattingOptions{},
			// SelectionRangeProvider:          &lsp.SelectionRangeRegistrationOptions{},
			DocumentOnTypeFormattingProvider: &lsp.DocumentOnTypeFormattingOptions{
				FirstTriggerCharacter: "\n",
			},
			RenameProvider: &lsp.RenameOptions{
				// PrepareProvider: true,
			},
			ExecuteCommandProvider: &lsp.ExecuteCommandOptions{
				Commands: []string{"clangd.applyFix", "clangd.applyTweak"},
			},
			// SelectionRangeProvider: &lsp.SelectionRangeOptions{},
			// CallHierarchyProvider: &lsp.CallHierarchyOptions{},
			// SemanticTokensProvider: &lsp.SemanticTokensOptions{
			// 	Legend: lsp.SemanticTokensLegend{
			// 		TokenTypes: []string{
			// 			"variable", "variable", "parameter", "function", "method",
			// 			"function", "property", "variable", "class", "enum",
			// 			"enumMember", "type", "dependent", "dependent", "namespace",
			// 			"typeParameter", "concept", "type", "macro", "comment",
			// 		},
			// 		TokenModifiers: []string{},
			// 	},
			// 	Range: false,
			// 	Full: &lsp.SemanticTokenFullOptions{
			// 		Delta: true,
			// 	},
			// },
			WorkspaceSymbolProvider: &lsp.WorkspaceSymbolOptions{},
		},
		ServerInfo: &lsp.InitializeResultServerInfo{
			Name:    "arduino-language-server",
			Version: globals.VersionInfo.VersionString,
		},
	}
	logger.Logf("initialization parameters: %s", string(lsp.EncodeMessage(resp)))
	return resp, nil
}

func (ls *INOLanguageServer) shutdownReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger) *jsonrpc.ResponseError {
	done := make(chan bool)
	go func() {
		ls.progressHandler.Shutdown()
		close(done)
	}()
	_, _ = ls.Clangd.conn.Shutdown(context.Background())
	ls.removeTemporaryFiles(logger)
	<-done
	return nil
}

func (ls *INOLanguageServer) textDocumentCompletionReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, ideParams *lsp.CompletionParams) (*lsp.CompletionList, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	clangTextDocPositionParams, err := ls.ide2ClangTextDocumentPositionParams(logger, ideParams.TextDocumentPositionParams)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	clangParams := &lsp.CompletionParams{
		TextDocumentPositionParams: clangTextDocPositionParams,
		Context:                    ideParams.Context,
		WorkDoneProgressParams:     ideParams.WorkDoneProgressParams,
		PartialResultParams:        ideParams.PartialResultParams,
	}

	clangCompletionList, clangErr, err := ls.Clangd.conn.TextDocumentCompletion(ctx, clangParams)
	if err != nil {
		logger.Logf("clangd connection error: %v", err)
		ls.Close()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response error: %v", clangErr.AsError())
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	ideCompletionList := &lsp.CompletionList{
		IsIncomplete: clangCompletionList.IsIncomplete,
	}
	for _, clangItem := range clangCompletionList.Items {
		if strings.HasPrefix(clangItem.InsertText, "_") {
			// XXX: Should be really ignored?
			continue
		}

		var ideTextEdit *lsp.TextEdit
		if clangItem.TextEdit != nil {
			if ideURI, _ideTextEdit, isPreprocessed, err := ls.cpp2inoTextEdit(logger, clangParams.TextDocument.URI, *clangItem.TextEdit); err != nil {
				logger.Logf("Error converting textedit: %s", err)
				return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
			} else if ideURI != ideParams.TextDocument.URI || isPreprocessed {
				err := fmt.Errorf("text edit is in preprocessed section or is mapped to another file")
				logger.Logf("Error converting textedit: %s", err)
				return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
			} else {
				ideTextEdit = &_ideTextEdit
			}
		}
		var ideAdditionalTextEdits []lsp.TextEdit
		if len(clangItem.AdditionalTextEdits) > 0 {
			_ideAdditionalTextEdits, err := ls.cland2IdeTextEdits(logger, clangParams.TextDocument.URI, clangItem.AdditionalTextEdits)
			if err != nil {
				logger.Logf("Error converting textedit: %s", err)
				return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
			}
			ideAdditionalTextEdits = _ideAdditionalTextEdits[ideParams.TextDocument.URI]
		}

		var ideCommand *lsp.Command
		if clangItem.Command != nil {
			c := ls.clang2IdeCommand(logger, *clangItem.Command)
			if c == nil {
				continue // Skit item with unsupported command conversion
			}
			ideCommand = c
		}

		ideCompletionList.Items = append(ideCompletionList.Items, lsp.CompletionItem{
			Label:               clangItem.Label,
			LabelDetails:        clangItem.LabelDetails,
			Kind:                clangItem.Kind,
			Tags:                clangItem.Tags,
			Detail:              clangItem.Detail,
			Documentation:       clangItem.Documentation,
			Deprecated:          clangItem.Deprecated,
			Preselect:           clangItem.Preselect,
			SortText:            clangItem.SortText,
			FilterText:          clangItem.FilterText,
			InsertText:          clangItem.InsertText,
			InsertTextFormat:    clangItem.InsertTextFormat,
			InsertTextMode:      clangItem.InsertTextMode,
			CommitCharacters:    clangItem.CommitCharacters,
			Data:                clangItem.Data,
			Command:             ideCommand,
			TextEdit:            ideTextEdit,
			AdditionalTextEdits: ideAdditionalTextEdits,
		})
	}
	logger.Logf("<-- completion(%d items)", len(ideCompletionList.Items))
	return ideCompletionList, nil
}

func (ls *INOLanguageServer) textDocumentHoverReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, ideParams *lsp.HoverParams) (*lsp.Hover, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	clangTextDocPosition, err := ls.ide2ClangTextDocumentPositionParams(logger, ideParams.TextDocumentPositionParams)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	clangParams := &lsp.HoverParams{
		TextDocumentPositionParams: clangTextDocPosition,
		WorkDoneProgressParams:     ideParams.WorkDoneProgressParams,
	}
	clangResp, clangErr, err := ls.Clangd.conn.TextDocumentHover(ctx, clangParams)
	if err != nil {
		logger.Logf("clangd communication error: %v", err)
		ls.Close()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response error: %v", clangErr.AsError())
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	if clangResp == nil {
		logger.Logf("null response")
		return nil, nil
	}

	var ideRange *lsp.Range
	if clangResp.Range != nil {
		_, r, inPreprocessed, err := ls.clang2IdeRangeAndDocumentURI(logger, clangParams.TextDocument.URI, *clangResp.Range)
		if err != nil {
			logger.Logf("error during range conversion: %v", err)
			ls.Close()
			return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
		}
		if inPreprocessed {
			return nil, nil
		}
		ideRange = &r
	}
	ideResp := lsp.Hover{
		Contents: clangResp.Contents,
		Range:    ideRange,
	}
	logger.Logf("Hover content: %s", strconv.Quote(ideResp.Contents.Value))
	return &ideResp, nil
}

func (ls *INOLanguageServer) textDocumentSignatureHelpReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, ideParams *lsp.SignatureHelpParams) (*lsp.SignatureHelp, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	clangTextDocumentPosition, err := ls.ide2ClangTextDocumentPositionParams(logger, ideParams.TextDocumentPositionParams)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	clangParams := &lsp.SignatureHelpParams{
		TextDocumentPositionParams: clangTextDocumentPosition,
		WorkDoneProgressParams:     ideParams.WorkDoneProgressParams,
		Context:                    ideParams.Context,
	}
	clangSignatureHelp, clangErr, err := ls.Clangd.conn.TextDocumentSignatureHelp(ctx, clangParams)
	if err != nil {
		logger.Logf("clangd communication error: %v", err)
		ls.Close()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response error: %v", clangErr.AsError())
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	// No need to convert back to inoSignatureHelp
	ideSignatureHelp := clangSignatureHelp
	return ideSignatureHelp, nil
}

func (ls *INOLanguageServer) textDocumentDefinitionReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, ideParams *lsp.DefinitionParams) ([]lsp.Location, []lsp.LocationLink, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	clangTextDocPosition, err := ls.ide2ClangTextDocumentPositionParams(logger, ideParams.TextDocumentPositionParams)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	clangParams := &lsp.DefinitionParams{
		TextDocumentPositionParams: clangTextDocPosition,
		WorkDoneProgressParams:     ideParams.WorkDoneProgressParams,
		PartialResultParams:        ideParams.PartialResultParams,
	}
	clangLocations, clangLocationLinks, clangErr, err := ls.Clangd.conn.TextDocumentDefinition(ctx, clangParams)
	if err != nil {
		logger.Logf("clangd communication error: %v", err)
		ls.Close()
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response error: %v", clangErr.AsError())
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	var ideLocations []lsp.Location
	if clangLocations != nil {
		ideLocations, err = ls.clang2IdeLocationsArray(logger, clangLocations)
		if err != nil {
			logger.Logf("Error: %v", err)
			ls.Close()
			return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
		}
	}

	var ideLocationLinks []lsp.LocationLink
	if clangLocationLinks != nil {
		panic("unimplemented")
	}

	return ideLocations, ideLocationLinks, nil
}

func (ls *INOLanguageServer) textDocumentTypeDefinitionReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, ideParams *lsp.TypeDefinitionParams) ([]lsp.Location, []lsp.LocationLink, *jsonrpc.ResponseError) {
	// XXX: This capability is not advertised in the initialization message (clangd
	// does not advertise it either, so maybe we should just not implement it)
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	cppTextDocumentPosition, err := ls.ide2ClangTextDocumentPositionParams(logger, ideParams.TextDocumentPositionParams)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	clangParams := &lsp.TypeDefinitionParams{
		TextDocumentPositionParams: cppTextDocumentPosition,
		WorkDoneProgressParams:     ideParams.WorkDoneProgressParams,
		PartialResultParams:        ideParams.PartialResultParams,
	}
	clangLocations, clangLocationLinks, clangErr, err := ls.Clangd.conn.TextDocumentTypeDefinition(ctx, clangParams)
	if err != nil {
		logger.Logf("clangd communication error: %v", err)
		ls.Close()
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response error: %v", clangErr.AsError())
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	var ideLocations []lsp.Location
	if clangLocations != nil {
		ideLocations, err = ls.clang2IdeLocationsArray(logger, clangLocations)
		if err != nil {
			logger.Logf("Error: %v", err)
			ls.Close()
			return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
		}
	}

	var ideLocationLinks []lsp.LocationLink
	if clangLocationLinks != nil {
		panic("unimplemented")
	}

	return ideLocations, ideLocationLinks, nil
}

func (ls *INOLanguageServer) textDocumentImplementationReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, ideParams *lsp.ImplementationParams) ([]lsp.Location, []lsp.LocationLink, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	clangTextDocumentPosition, err := ls.ide2ClangTextDocumentPositionParams(logger, ideParams.TextDocumentPositionParams)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	clangParams := &lsp.ImplementationParams{
		TextDocumentPositionParams: clangTextDocumentPosition,
		WorkDoneProgressParams:     ideParams.WorkDoneProgressParams,
		PartialResultParams:        ideParams.PartialResultParams,
	}
	clangLocations, clangLocationLinks, clangErr, err := ls.Clangd.conn.TextDocumentImplementation(ctx, clangParams)
	if err != nil {
		logger.Logf("clangd communication error: %v", err)
		ls.Close()
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response error: %v", clangErr.AsError())
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	var ideLocations []lsp.Location
	if clangLocations != nil {
		ideLocations, err = ls.clang2IdeLocationsArray(logger, clangLocations)
		if err != nil {
			logger.Logf("Error: %v", err)
			ls.Close()
			return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
		}
	}

	var inoLocationLinks []lsp.LocationLink
	if clangLocationLinks != nil {
		panic("unimplemented")
	}

	return ideLocations, inoLocationLinks, nil
}

func (ls *INOLanguageServer) textDocumentDocumentHighlightReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, ideParams *lsp.DocumentHighlightParams) ([]lsp.DocumentHighlight, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	clangTextDocumentPosition, err := ls.ide2ClangTextDocumentPositionParams(logger, ideParams.TextDocumentPositionParams)
	if err != nil {
		logger.Logf("ERROR: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	clangURI := clangTextDocumentPosition.TextDocument.URI

	clangParams := &lsp.DocumentHighlightParams{
		TextDocumentPositionParams: clangTextDocumentPosition,
		WorkDoneProgressParams:     ideParams.WorkDoneProgressParams,
		PartialResultParams:        ideParams.PartialResultParams,
	}
	clangHighlights, clangErr, err := ls.Clangd.conn.TextDocumentDocumentHighlight(ctx, clangParams)
	if err != nil {
		logger.Logf("clangd communication ERROR: %v", err)
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
		ideHighlight, inPreprocessed, err := ls.clang2IdeDocumentHighlight(logger, clangHighlight, clangURI)
		if inPreprocessed {
			continue
		}
		if err != nil {
			logger.Logf("ERROR converting highlight %s:%s: %s", clangURI, clangHighlight.Range, err)
			return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
		}
		ideHighlights = append(ideHighlights, ideHighlight)
	}
	return ideHighlights, nil
}

func (ls *INOLanguageServer) textDocumentDocumentSymbolReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, ideParams *lsp.DocumentSymbolParams) ([]lsp.DocumentSymbol, []lsp.SymbolInformation, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	// Convert request for clang
	clangTextDocument, err := ls.ide2ClangTextDocumentIdentifier(logger, ideParams.TextDocument)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	// Send request to clang
	clangParams := &lsp.DocumentSymbolParams{
		TextDocument:           clangTextDocument,
		WorkDoneProgressParams: ideParams.WorkDoneProgressParams,
		PartialResultParams:    ideParams.PartialResultParams,
	}
	clangDocSymbols, clangSymbolsInformation, clangErr, err := ls.Clangd.conn.TextDocumentDocumentSymbol(ctx, clangParams)
	if err != nil {
		logger.Logf("clangd communication error: %v", err)
		ls.Close()
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response error: %v", clangErr.AsError())
		return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	// Convert response for IDE
	var ideDocSymbols []lsp.DocumentSymbol
	if clangDocSymbols != nil {
		s, err := ls.clang2IdeDocumentSymbols(logger, clangDocSymbols, clangParams.TextDocument.URI, ideParams.TextDocument.URI)
		if err != nil {
			logger.Logf("Error: %s", err)
			ls.Close()
			return nil, nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
		}
		ideDocSymbols = s
	}
	var ideSymbolsInformation []lsp.SymbolInformation
	if clangSymbolsInformation != nil {
		ideSymbolsInformation = ls.clang2IdeSymbolsInformation(logger, clangSymbolsInformation)
	}
	return ideDocSymbols, ideSymbolsInformation, nil
}

func (ls *INOLanguageServer) textDocumentCodeActionReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, ideParams *lsp.CodeActionParams) ([]lsp.CommandOrCodeAction, *jsonrpc.ResponseError) {
	ls.readLock(logger, true)
	defer ls.readUnlock(logger)

	ideTextDocument := ideParams.TextDocument
	ideURI := ideTextDocument.URI
	logger.Logf("--> codeAction(%s:%s)", ideTextDocument, ideParams.Range.Start)

	clangURI, clangRange, err := ls.ide2ClangRange(logger, ideURI, ideParams.Range)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	clangContext, err := ls.ide2ClangCodeActionContext(logger, ideURI, ideParams.Context)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	clangParams := &lsp.CodeActionParams{
		WorkDoneProgressParams: ideParams.WorkDoneProgressParams,
		PartialResultParams:    ideParams.PartialResultParams,
		TextDocument:           lsp.TextDocumentIdentifier{URI: clangURI},
		Range:                  clangRange,
		Context:                clangContext,
	}
	logger.Logf("    --> codeAction(%s:%s)", clangParams.TextDocument, ideParams.Range.Start)

	clangCommandsOrCodeActions, clangErr, err := ls.Clangd.conn.TextDocumentCodeAction(ctx, clangParams)
	if err != nil {
		logger.Logf("clangd communication error: %v", err)
		ls.Close()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response error: %v", clangErr.AsError())
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	// TODO: Create a function for this one?
	ideCommandsOrCodeActions := []lsp.CommandOrCodeAction{}
	if clangCommandsOrCodeActions != nil {
		return ideCommandsOrCodeActions, nil
	}
	logger.Logf("    <-- codeAction(%d elements)", len(clangCommandsOrCodeActions))
	for _, clangItem := range clangCommandsOrCodeActions {
		ideItem := lsp.CommandOrCodeAction{}
		switch i := clangItem.Get().(type) {
		case lsp.Command:
			logger.Logf("        > Command: %s", i.Title)
			ideCommand := ls.clang2IdeCommand(logger, i)
			if ideCommand == nil {
				continue // Skip unsupported command
			}
			ideItem.Set(*ideCommand)
		case lsp.CodeAction:
			logger.Logf("        > CodeAction: %s", i.Title)
			ideCodeAction := ls.clang2IdeCodeAction(logger, i, ideURI)
			if ideCodeAction == nil {
				continue // Skip unsupported code action
			}
			ideItem.Set(*ideCodeAction)
		}
		ideCommandsOrCodeActions = append(ideCommandsOrCodeActions, ideItem)
	}
	logger.Logf("<-- codeAction(%d elements)", len(ideCommandsOrCodeActions))
	return ideCommandsOrCodeActions, nil
}

func (ls *INOLanguageServer) textDocumentFormattingReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, ideParams *lsp.DocumentFormattingParams) ([]lsp.TextEdit, *jsonrpc.ResponseError) {
	ls.writeLock(logger, true)
	defer ls.writeUnlock(logger)

	ideTextDocument := ideParams.TextDocument
	ideURI := ideTextDocument.URI

	clangTextDocument, err := ls.ide2ClangTextDocumentIdentifier(logger, ideTextDocument)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	clangURI := clangTextDocument.URI

	cleanup, err := ls.createClangdFormatterConfig(logger, clangURI)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	defer cleanup()

	clangParams := &lsp.DocumentFormattingParams{
		WorkDoneProgressParams: ideParams.WorkDoneProgressParams,
		Options:                ideParams.Options,
		TextDocument:           clangTextDocument,
	}
	clangEdits, clangErr, err := ls.Clangd.conn.TextDocumentFormatting(ctx, clangParams)
	if err != nil {
		logger.Logf("clangd communication error: %v", err)
		ls.Close()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response error: %v", clangErr.AsError())
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	if clangEdits == nil {
		return nil, nil
	}

	ideEdits, err := ls.cland2IdeTextEdits(logger, clangURI, clangEdits)
	if err != nil {
		logger.Logf("ERROR converting textEdits: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	// Edits may span over multiple .ino files, filter only the edits relative to the currently displayed file
	inoEdits, ok := ideEdits[ideURI]
	if !ok {
		return []lsp.TextEdit{}, nil
	}
	return inoEdits, nil
}

func (ls *INOLanguageServer) textDocumentRangeFormattingReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, ideParams *lsp.DocumentRangeFormattingParams) ([]lsp.TextEdit, *jsonrpc.ResponseError) {
	ls.writeLock(logger, true)
	defer ls.writeUnlock(logger)

	ideURI := ideParams.TextDocument.URI
	clangURI, clangRange, err := ls.ide2ClangRange(logger, ideURI, ideParams.Range)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	clangParams := &lsp.DocumentRangeFormattingParams{
		WorkDoneProgressParams: ideParams.WorkDoneProgressParams,
		Options:                ideParams.Options,
		TextDocument:           lsp.TextDocumentIdentifier{URI: clangURI},
		Range:                  clangRange,
	}

	cleanup, e := ls.createClangdFormatterConfig(logger, clangURI)
	if e != nil {
		logger.Logf("cannot create formatter config file: %v", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	defer cleanup()

	clangEdits, clangErr, err := ls.Clangd.conn.TextDocumentRangeFormatting(ctx, clangParams)
	if err != nil {
		logger.Logf("clangd communication error: %v", err)
		ls.Close()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response error: %v", clangErr.AsError())
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	if clangEdits == nil {
		return nil, nil
	}

	sketchEdits, err := ls.cland2IdeTextEdits(logger, clangURI, clangEdits)
	if err != nil {
		logger.Logf("ERROR converting textEdits: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	// Edits may span over multiple .ino files, filter only the edits relative to the currently displayed file
	inoEdits, ok := sketchEdits[ideURI]
	if !ok {
		return []lsp.TextEdit{}, nil
	}
	return inoEdits, nil
}

func (ls *INOLanguageServer) initializedNotifFromIDE(logger jsonrpc.FunctionLogger, ideParams *lsp.InitializedParams) {
	logger.Logf("Notification is not propagated to clangd")
}

func (ls *INOLanguageServer) exitNotifFromIDE(logger jsonrpc.FunctionLogger) {
	ls.Clangd.conn.Exit()
	logger.Logf("Arduino Language Server is exiting.")
	ls.Close()
}

func (ls *INOLanguageServer) textDocumentDidOpenNotifFromIDE(logger jsonrpc.FunctionLogger, ideParam *lsp.DidOpenTextDocumentParams) {
	ls.writeLock(logger, true)
	defer ls.writeUnlock(logger)

	ideTextDocItem := ideParam.TextDocument
	clangURI, _, err := ls.ide2ClangDocumentURI(logger, ideTextDocItem.URI)
	if err != nil {
		logger.Logf("Error: %s", err)
		return
	}

	if ls.ideURIIsPartOfTheSketch(ideTextDocItem.URI) {
		if !clangURI.AsPath().Exist() {
			ls.triggerRebuildAndWait(logger)
		}
	}

	// Add the TextDocumentItem in the tracked files list
	ls.trackedIdeDocs[ideTextDocItem.URI.AsPath().String()] = ideTextDocItem

	// If we are tracking a .ino...
	if ideTextDocItem.URI.Ext() == ".ino" {
		ls.sketchTrackedFilesCount++
		logger.Logf("Increasing .ino tracked files count to %d", ls.sketchTrackedFilesCount)

		// Notify clangd that sketchCpp has been opened only once
		if ls.sketchTrackedFilesCount != 1 {
			logger.Logf("Clang already notified, do not notify it anymore")
			return
		}
	}

	clangTextDocItem := lsp.TextDocumentItem{
		URI: clangURI,
	}
	if ls.clangURIRefersToIno(clangURI) {
		clangTextDocItem.LanguageID = "cpp"
		clangTextDocItem.Text = ls.sketchMapper.CppText.Text
		clangTextDocItem.Version = ls.sketchMapper.CppText.Version
	} else {
		clangText, err := clangURI.AsPath().ReadFile()
		if err != nil {
			logger.Logf("Error opening sketch file %s: %s", clangURI.AsPath(), err)
		}
		clangTextDocItem.LanguageID = ideTextDocItem.LanguageID
		clangTextDocItem.Version = ideTextDocItem.Version
		clangTextDocItem.Text = string(clangText)
	}

	if err := ls.Clangd.conn.TextDocumentDidOpen(&lsp.DidOpenTextDocumentParams{
		TextDocument: clangTextDocItem,
	}); err != nil {
		// Exit the process and trigger a restart by the client in case of a severe error
		logger.Logf("Error sending notification to clangd server: %v", err)
		logger.Logf("Please restart the language server.")
		ls.Close()
	}
}

func (ls *INOLanguageServer) textDocumentDidChangeNotifFromIDE(logger jsonrpc.FunctionLogger, ideParams *lsp.DidChangeTextDocumentParams) {
	ls.writeLock(logger, true)
	defer ls.writeUnlock(logger)

	ls.triggerRebuild()

	logger.Logf("didChange(%s)", ideParams.TextDocument)
	for _, change := range ideParams.ContentChanges {
		logger.Logf("  > %s", change)
	}

	// Clear all RangeLengths: it's a deprecated field and if the byte count is wrong the
	// source text file will be unloaded from clangd without notice, leading to a "non-added
	// document" error for all subsequent requests.
	// https://github.com/clangd/clangd/issues/717#issuecomment-793220007
	for i := range ideParams.ContentChanges {
		ideParams.ContentChanges[i].RangeLength = nil
	}

	ideTextDocIdentifier := ideParams.TextDocument

	// Apply the change to the tracked sketch file.
	trackedIdeDocID := ideTextDocIdentifier.URI.AsPath().String()
	if doc, ok := ls.trackedIdeDocs[trackedIdeDocID]; !ok {
		logger.Logf("Error: %s", &UnknownURIError{ideTextDocIdentifier.URI})
		return
	} else if updatedDoc, err := textedits.ApplyLSPTextDocumentContentChangeEvent(doc, ideParams); err != nil {
		logger.Logf("Error: %s", err)
		return
	} else {
		ls.trackedIdeDocs[trackedIdeDocID] = updatedDoc
		logger.Logf("-----Tracked SKETCH file-----\n" + updatedDoc.Text + "\n-----------------------------")
	}

	clangChanges := []lsp.TextDocumentContentChangeEvent{}
	var clangURI *lsp.DocumentURI
	var clangParams *lsp.DidChangeTextDocumentParams
	for _, ideChange := range ideParams.ContentChanges {
		if ideChange.Range == nil {
			panic("full-text change not implemented")
		}

		clangRangeURI, clangRange, err := ls.ide2ClangRange(logger, ideTextDocIdentifier.URI, *ideChange.Range)
		if err != nil {
			logger.Logf("Error: %s", err)
			return
		}

		// all changes should refer to the same URI
		if clangURI == nil {
			clangURI = &clangRangeURI
		} else if *clangURI != clangRangeURI {
			logger.Logf("Error: change maps to %s URI, but %s was expected", clangRangeURI, *clangURI)
			return
		}

		// If we are applying changes to a .ino, update the sketchmapper
		if ideTextDocIdentifier.URI.Ext() == ".ino" {
			_ = ls.sketchMapper.ApplyTextChange(ideTextDocIdentifier.URI, ideChange)
		}

		clangChanges = append(clangChanges, lsp.TextDocumentContentChangeEvent{
			Range:       &clangRange,
			RangeLength: ideChange.RangeLength,
			Text:        ideChange.Text,
		})
	}

	clangVersion := ideTextDocIdentifier.Version
	if ideTextDocIdentifier.URI.Ext() == ".ino" {
		// If changes are applied to a .ino file we increment the global .ino.cpp versioning
		// for each increment of the single .ino file.
		clangVersion = ls.sketchMapper.CppText.Version
		ls.sketchMapper.DebugLogAll()
	}

	// build a cpp equivalent didChange request
	clangParams = &lsp.DidChangeTextDocumentParams{
		TextDocument: lsp.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: lsp.TextDocumentIdentifier{URI: *clangURI},
			Version:                clangVersion,
		},
		ContentChanges: clangChanges,
	}

	logger.Logf("to Clang: didChange(%s)", clangParams.TextDocument)
	for _, change := range clangParams.ContentChanges {
		logger.Logf("            > %s", change)
	}
	if err := ls.Clangd.conn.TextDocumentDidChange(clangParams); err != nil {
		logger.Logf("Connection error with clangd server: %v", err)
		logger.Logf("Please restart the language server.")
		ls.Close()
	}
}

func (ls *INOLanguageServer) textDocumentDidSaveNotifFromIDE(logger jsonrpc.FunctionLogger, ideParams *lsp.DidSaveTextDocumentParams) {
	ls.writeLock(logger, true)
	defer ls.writeUnlock(logger)

	// clangd looks in the build directory (where a copy of the preprocessed sketch resides)
	// so we will not forward notification on saves in the sketch folder.
	logger.Logf("notification is not forwarded to clang")

	ls.triggerRebuild()
}

func (ls *INOLanguageServer) textDocumentDidCloseNotifFromIDE(logger jsonrpc.FunctionLogger, ideParams *lsp.DidCloseTextDocumentParams) {
	ls.writeLock(logger, true)
	defer ls.writeUnlock(logger)

	ls.triggerRebuild()

	inoIdentifier := ideParams.TextDocument
	if _, exist := ls.trackedIdeDocs[inoIdentifier.URI.AsPath().String()]; exist {
		delete(ls.trackedIdeDocs, inoIdentifier.URI.AsPath().String())
	} else {
		logger.Logf("didClose of untracked document: %s", inoIdentifier.URI)
		return
	}

	// If we are tracking a .ino...
	if inoIdentifier.URI.Ext() == ".ino" {
		ls.sketchTrackedFilesCount--
		logger.Logf("decreasing .ino tracked files count: %d", ls.sketchTrackedFilesCount)

		// notify clang that sketch.cpp.ino has been closed only once all .ino are closed
		if ls.sketchTrackedFilesCount != 0 {
			logger.Logf("--X Notification is not propagated to clangd")
			return
		}
	}

	clangIdentifier, err := ls.ide2ClangTextDocumentIdentifier(logger, inoIdentifier)
	if err != nil {
		logger.Logf("Error: %s", err)
	}
	clangParams := &lsp.DidCloseTextDocumentParams{
		TextDocument: clangIdentifier,
	}

	logger.Logf("--> didClose(%s)", clangParams.TextDocument)
	if err := ls.Clangd.conn.TextDocumentDidClose(clangParams); err != nil {
		// Exit the process and trigger a restart by the client in case of a severe error
		logger.Logf("Error sending notification to clangd server: %v", err)
		logger.Logf("Please restart the language server.")
		ls.Close()
	}
}

func (ls *INOLanguageServer) fullBuildCompletedFromIDE(logger jsonrpc.FunctionLogger, params *DidCompleteBuildParams) {
	ls.writeLock(logger, true)
	defer ls.writeUnlock(logger)

	ls.CopyFullBuildResults(logger, params.BuildOutputURI.AsPath())
	ls.triggerRebuild()
}

// CopyFullBuildResults copies the results of a full build in the LS workspace
func (ls *INOLanguageServer) CopyFullBuildResults(logger jsonrpc.FunctionLogger, buildPath *paths.Path) {
	fromCache := buildPath.Join("libraries.cache")
	toCache := ls.buildPath.Join("libraries.cache")
	if err := fromCache.CopyTo(toCache); err != nil {
		logger.Logf("ERROR: updating libraries.cache: %s", err)
	} else {
		logger.Logf("Updated 'libraries.cache'. Copied: %v to %v", fromCache, toCache)
	}
}

func (ls *INOLanguageServer) publishDiagnosticsNotifFromClangd(logger jsonrpc.FunctionLogger, clangParams *lsp.PublishDiagnosticsParams) {
	if ls.config.DisableRealTimeDiagnostics {
		logger.Logf("Ignored by configuration")
		return
	}

	ls.readLock(logger, false)
	defer ls.readUnlock(logger)

	logger.Logf("%s (%d diagnostics):", clangParams.URI, len(clangParams.Diagnostics))
	for _, diag := range clangParams.Diagnostics {
		logger.Logf("  > %s - %s: %s", diag.Range.Start, diag.Severity, string(diag.Code))
	}

	// the diagnostics on sketch.cpp.ino once mapped into their
	// .ino counter parts may span over multiple .ino files...
	allIdeParams, err := ls.clang2IdeDiagnostics(logger, clangParams)
	if err != nil {
		logger.Logf("Error converting diagnostics to .ino: %s", err)
		return
	}

	// If the incoming diagnostics are from sketch.cpp.ino then...
	if ls.clangURIRefersToIno(clangParams.URI) {
		// ...add all the new diagnostics...
		for ideInoURI := range allIdeParams {
			ls.ideInoDocsWithDiagnostics[ideInoURI] = true
		}

		// .. and cleanup all previous diagnostics that are no longer valid...
		for ideInoURI := range ls.ideInoDocsWithDiagnostics {
			if _, ok := allIdeParams[ideInoURI]; ok {
				continue
			}
			allIdeParams[ideInoURI] = &lsp.PublishDiagnosticsParams{
				URI:         ideInoURI,
				Diagnostics: []lsp.Diagnostic{},
			}
			delete(ls.ideInoDocsWithDiagnostics, ideInoURI)
		}
	}

	// Try to filter as much bogus errors as possible (due to wrong clang "driver" or missing
	// support for specific embedded CPU architecture).
	for _, ideParams := range allIdeParams {
		n := 0
		for _, ideDiag := range ideParams.Diagnostics {
			var code string
			_ = json.Unmarshal(ideDiag.Code, &code)
			switch code {
			case "":
				// Filter unknown non-string codes
			case "drv_unknown_argument_with_suggestion":
				// Skip errors like: "Unknown argument '-mlongcalls'; did you mean '-mlong-calls'?"
			case "drv_unknown_argument":
				// Skip errors like: "Unknown argument: '-mtext-section-literals'"
			default:
				ideParams.Diagnostics[n] = ideDiag
				n++
				continue
			}
			logger.Logf("filtered out diagnostic with error-code: %s", ideDiag.Code)
		}
		ideParams.Diagnostics = ideParams.Diagnostics[:n]
	}

	// Push back to IDE the converted diagnostics
	logger.Logf("diagnostics to IDE:")
	for _, ideParams := range allIdeParams {
		logger.Logf("  - %s (%d diagnostics):", ideParams.URI, len(ideParams.Diagnostics))
		for _, diag := range ideParams.Diagnostics {
			logger.Logf("    > %s - %s: %s", diag.Range.Start, diag.Severity, diag.Code)
		}
		if err := ls.IDE.conn.TextDocumentPublishDiagnostics(ideParams); err != nil {
			logger.Logf("Error sending diagnostics to IDE: %s", err)
			return
		}
	}
}

func (ls *INOLanguageServer) textDocumentRenameReqFromIDE(ctx context.Context, logger jsonrpc.FunctionLogger, ideParams *lsp.RenameParams) (*lsp.WorkspaceEdit, *jsonrpc.ResponseError) {
	ls.writeLock(logger, false)
	defer ls.writeUnlock(logger)

	clangTextDocPositionParams, err := ls.ide2ClangTextDocumentPositionParams(logger, ideParams.TextDocumentPositionParams)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	clangParams := &lsp.RenameParams{
		TextDocumentPositionParams: clangTextDocPositionParams,
		NewName:                    ideParams.NewName,
		WorkDoneProgressParams:     ideParams.WorkDoneProgressParams,
	}
	clangWorkspaceEdit, clangErr, err := ls.Clangd.conn.TextDocumentRename(ctx, clangParams)
	if err != nil {
		logger.Logf("clangd communication error: %v", err)
		ls.Close()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	if clangErr != nil {
		logger.Logf("clangd response error: %v", clangErr.AsError())
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: clangErr.AsError().Error()}
	}

	ideWorkspaceEdit, err := ls.clang2IdeWorkspaceEdit(logger, clangWorkspaceEdit)
	if err != nil {
		logger.Logf("Error: %s", err)
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}

	// Check if all edits belongs to the sketch
	for ideURI := range ideWorkspaceEdit.Changes {
		if !ls.ideURIIsPartOfTheSketch(ideURI) {
			return nil, &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInvalidParams, Message: "Could not rename symbol, it requires changes outside the sketch."}
		}
	}
	return ideWorkspaceEdit, nil
}

func (ls *INOLanguageServer) ideURIIsPartOfTheSketch(ideURI lsp.DocumentURI) bool {
	res, _ := ideURI.AsPath().IsInsideDir(ls.sketchRoot)
	return res
}

func (ls *INOLanguageServer) progressNotifFromClangd(logger jsonrpc.FunctionLogger, progress *lsp.ProgressParams) {
	var token string
	if err := json.Unmarshal(progress.Token, &token); err != nil {
		logger.Logf("error decoding progress token: %s", err)
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

func (ls *INOLanguageServer) windowWorkDoneProgressCreateReqFromClangd(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.WorkDoneProgressCreateParams) *jsonrpc.ResponseError {
	var token string
	if err := json.Unmarshal(params.Token, &token); err != nil {
		logger.Logf("error decoding progress token: %s", err)
		return &jsonrpc.ResponseError{Code: jsonrpc.ErrorCodesInternalError, Message: err.Error()}
	}
	ls.progressHandler.Create(token)
	return nil
}

func (ls *INOLanguageServer) setTraceNotifFromIDE(logger jsonrpc.FunctionLogger, params *lsp.SetTraceParams) {
	logger.Logf("Notification level set to: %s", params.Value)
	ls.Clangd.conn.SetTrace(params)
}

func (ls *INOLanguageServer) removeTemporaryFiles(logger jsonrpc.FunctionLogger) {
	ls.removeTempMutex.Lock()
	defer ls.removeTempMutex.Unlock()

	if ls.tempDir == nil {
		// Nothing to remove
		return
	}

	// Start a detached process to remove the temp files
	cwd, err := os.Getwd()
	if err != nil {
		logger.Logf("Error getting current working directory: %s", err)
		return
	}
	cmd := exec.Command(os.Args[0], "remove-temp-files", ls.tempDir.String())
	cmd.Dir = cwd
	if err := cmd.Start(); err != nil {
		logger.Logf("Error starting remove-temp-files process: %s", err)
		return
	}

	// The process is now started, we can reset the paths
	ls.buildPath, ls.fullBuildPath, ls.buildSketchRoot, ls.tempDir = nil, nil, nil, nil

	// Detach the process so it can continue running even if the parent process exits
	if err := cmd.Process.Release(); err != nil {
		logger.Logf("Error detaching remove-temp-files process: %s", err)
		return
	}
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
}

// CloseNotify returns a channel that is closed when the InoHandler is closed
func (ls *INOLanguageServer) CloseNotify() <-chan bool {
	return ls.closing
}

func (ls *INOLanguageServer) extractDataFolderFromArduinoCLI(logger jsonrpc.FunctionLogger) (*paths.Path, error) {
	var dataDir string
	if ls.config.CliPath == nil {
		// Establish a connection with the arduino-cli gRPC server
		conn, err := grpc.Dial(
			ls.config.CliDaemonAddress,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithBlock())
		if err != nil {
			return nil, fmt.Errorf("error connecting to arduino-cli rpc server: %w", err)
		}
		defer conn.Close()
		client := rpc.NewArduinoCoreServiceClient(conn)

		resp, err := client.SettingsGetValue(context.Background(), &rpc.SettingsGetValueRequest{
			Key: "directories.data",
		})
		if err != nil {
			return nil, fmt.Errorf("error getting arduino data dir: %w", err)
		}
		if err := json.Unmarshal([]byte(resp.GetEncodedValue()), &dataDir); err != nil {
			return nil, fmt.Errorf("error getting arduino data dir: %w", err)
		}
		logger.Logf("Arduino Data Dir -> %s", dataDir)
	} else {
		args := []string{
			"--config-file", ls.config.CliConfigPath.String(),
			"config", "get", "directories.data",
			"--json",
		}
		cmd, err := paths.NewProcessFromPath(nil, ls.config.CliPath, args...)
		if err != nil {
			return nil, errors.Errorf("running %s: %s", strings.Join(args, " "), err)
		}
		cmdOutput := &bytes.Buffer{}
		cmd.RedirectStdoutTo(cmdOutput)
		logger.Logf("running: %s", strings.Join(args, " "))
		if err := cmd.Run(); err != nil {
			return nil, errors.Errorf("running %s: %s", strings.Join(args, " "), err)
		}

		var res string
		if err := json.Unmarshal(cmdOutput.Bytes(), &res); err != nil {
			return nil, errors.Errorf("parsing arduino-cli output: %s", err)
		}
		// Return only the build path
		logger.Logf("Arduino Data Dir -> %s", res)
		dataDir = res
	}

	dataDirPath := paths.New(dataDir)
	return dataDirPath.Canonical(), nil
}

func (ls *INOLanguageServer) clang2IdeCodeAction(logger jsonrpc.FunctionLogger, clangCodeAction lsp.CodeAction, origIdeURI lsp.DocumentURI) *lsp.CodeAction {
	ideCodeAction := &lsp.CodeAction{
		Title:       clangCodeAction.Title,
		Kind:        clangCodeAction.Kind,
		Diagnostics: clangCodeAction.Diagnostics,
		IsPreferred: clangCodeAction.IsPreferred,
		Disabled:    clangCodeAction.Disabled,
		Edit:        ls.cpp2inoWorkspaceEdit(logger, clangCodeAction.Edit),
	}
	if clangCodeAction.Command != nil {
		inoCommand := ls.clang2IdeCommand(logger, *clangCodeAction.Command)
		if inoCommand == nil {
			return nil
		}
		ideCodeAction.Command = inoCommand
	}
	if origIdeURI.Ext() == ".ino" {
		for i, diag := range ideCodeAction.Diagnostics {
			_, ideCodeAction.Diagnostics[i].Range = ls.sketchMapper.CppToInoRange(diag.Range)
		}
	}
	return ideCodeAction
}

func (ls *INOLanguageServer) clang2IdeCommand(logger jsonrpc.FunctionLogger, clangCommand lsp.Command) *lsp.Command {
	switch clangCommand.Command {
	case "clangd.applyTweak":
		logger.Logf("> Command: clangd.applyTweak")
		ideCommand := &lsp.Command{
			Title:     clangCommand.Title,
			Command:   clangCommand.Command,
			Arguments: clangCommand.Arguments,
		}
		for i := range clangCommand.Arguments {
			v := struct {
				TweakID   string          `json:"tweakID"`
				File      lsp.DocumentURI `json:"file"`
				Selection lsp.Range       `json:"selection"`
			}{}

			if err := json.Unmarshal(clangCommand.Arguments[0], &v); err == nil {
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
				panic("Internal Error: json conversion of codeAction command arguments")
			}
			ideCommand.Arguments[i] = converted
		}
		return ideCommand
	default:
		logger.Logf("ERROR: could not convert Command '%s'", clangCommand.Command)
		return nil
	}
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

		// ...otherwise convert edits to the sketch.ino.cpp into multiple .ino edits
		for _, edit := range edits {
			inoURI, inoRange, inPreprocessed, err := ls.clang2IdeRangeAndDocumentURI(logger, editURI, edit.Range)
			if err != nil {
				logger.Logf("    error converting edit %s:%s: %s", editURI, edit.Range, err)
				continue
			}
			if inPreprocessed {
				// XXX: ignore
				logger.Logf("    ignored in-preprocessed-section change")
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

func (ls *INOLanguageServer) cpp2inoTextEdit(logger jsonrpc.FunctionLogger, cppURI lsp.DocumentURI, cppEdit lsp.TextEdit) (lsp.DocumentURI, lsp.TextEdit, bool, error) {
	inoURI, inoRange, inPreprocessed, err := ls.clang2IdeRangeAndDocumentURI(logger, cppURI, cppEdit.Range)

	if err != nil {
		if strings.HasPrefix(cppEdit.NewText, "\n") && cppEdit.Range.Start.Line < cppEdit.Range.End.Line {
			// Special case: the text-edit may start from the very end of a not-ino section and fallthrough
			// in the .ino section with a '\n...' at the beginning of the replacement text.
			nextLine := lsp.Position{Line: cppEdit.Range.Start.Line + 1, Character: 0}
			startOffset, err1 := textedits.GetOffset(ls.sketchMapper.CppText.Text, cppEdit.Range.Start)
			nextOffset, err2 := textedits.GetOffset(ls.sketchMapper.CppText.Text, nextLine)
			if err1 == nil && err2 == nil && startOffset+1 == nextOffset {
				// In this can we can generate an equivalent text-edit that fits entirely in the .ino section
				// by removing the redundant '\n' and by offsetting the start location to the beginning of the
				// next line.
				cppEdit.Range.Start = nextLine
				cppEdit.NewText = cppEdit.NewText[1:]
				return ls.cpp2inoTextEdit(logger, cppURI, cppEdit)
			}
		}
	}

	inoEdit := cppEdit
	inoEdit.Range = inoRange
	return inoURI, inoEdit, inPreprocessed, err
}

// UnknownURIError is an error when an URI is not recognized
type UnknownURIError struct {
	URI lsp.DocumentURI
}

func (e *UnknownURIError) Error() string {
	return "Document is not available: " + e.URI.String()
}
