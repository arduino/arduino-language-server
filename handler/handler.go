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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arduino/arduino-cli/arduino/builder"
	"github.com/arduino/arduino-cli/executils"
	"github.com/arduino/arduino-language-server/handler/sourcemapper"
	"github.com/arduino/arduino-language-server/handler/textutils"
	"github.com/arduino/arduino-language-server/lsp"
	"github.com/arduino/arduino-language-server/streams"
	"github.com/arduino/go-paths-helper"
	"github.com/fatih/color"
	"github.com/pkg/errors"
	"github.com/sourcegraph/jsonrpc2"
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
	StdioConn  *jsonrpc2.Conn
	ClangdConn *jsonrpc2.Conn

	stdioNotificationCount  int64
	clangdNotificationCount int64
	progressHandler         *ProgressProxyHandler

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

	config lsp.BoardConfig
}

var yellow = color.New(color.FgHiYellow)

func (handler *InoHandler) dataLock(msg string) {
	handler.dataMux.Lock()
	log.Println(msg + yellow.Sprintf(" locked"))
}

func (handler *InoHandler) dataUnlock(msg string) {
	log.Println(msg + yellow.Sprintf(" unlocked"))
	handler.dataMux.Unlock()
}

func (handler *InoHandler) dataRLock(msg string) {
	handler.dataMux.RLock()
	log.Println(msg + yellow.Sprintf(" read-locked"))
}

func (handler *InoHandler) dataRUnlock(msg string) {
	log.Println(msg + yellow.Sprintf(" read-unlocked"))
	handler.dataMux.RUnlock()
}

func (handler *InoHandler) waitClangdStart(prefix string) error {
	if handler.ClangdConn != nil {
		return nil
	}

	log.Printf(prefix + "(throttled: waiting for clangd)")
	log.Println(prefix + yellow.Sprintf(" unlocked (waiting clangd)"))
	handler.clangdStarted.Wait()
	log.Println(prefix + yellow.Sprintf(" locked (waiting clangd)"))

	if handler.ClangdConn == nil {
		log.Printf(prefix + "clangd startup failed: aborting call")
		return errors.New("could not start clangd, aborted")
	}
	return nil
}

// NewInoHandler creates and configures an InoHandler.
func NewInoHandler(stdio io.ReadWriteCloser, board lsp.Board) *InoHandler {
	handler := &InoHandler{
		docs:                   map[string]*lsp.TextDocumentItem{},
		inoDocsWithDiagnostics: map[lsp.DocumentURI]bool{},
		closing:                make(chan bool),
		config: lsp.BoardConfig{
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
		log.Println("Initial board configuration:", board)
		log.Println("Language server build path:", handler.buildPath)
		log.Println("Language server build sketch root:", handler.buildSketchRoot)
	}

	stdStream := jsonrpc2.NewBufferedStream(stdio, jsonrpc2.VSCodeObjectCodec{})
	var stdHandler jsonrpc2.Handler = jsonrpc2.HandlerWithError(handler.HandleMessageFromIDE)
	handler.StdioConn = jsonrpc2.NewConn(context.Background(), stdStream, stdHandler,
		jsonrpc2.OnRecv(streams.JSONRPCConnLogOnRecv("IDE --> LS     CL:")),
		jsonrpc2.OnSend(streams.JSONRPCConnLogOnSend("IDE <-- LS     CL:")),
	)

	handler.progressHandler = NewProgressProxy(handler.StdioConn)

	go handler.rebuildEnvironmentLoop()
	return handler
}

// FileData gathers information on a .ino source file.
type FileData struct {
	sourceText string
	sourceURI  lsp.DocumentURI
	targetURI  lsp.DocumentURI
	sourceMap  *sourcemapper.InoMapper
	version    int
}

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
		log.Printf("removing buildpath")
		handler.buildPath.RemoveAll()
		handler.buildPath = nil
	}
}

// HandleMessageFromIDE handles a message received from the IDE client (via stdio).
func (handler *InoHandler) HandleMessageFromIDE(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error) {
	defer streams.CatchAndLogPanic()

	prefix := "IDE --> "
	if req.Notif {
		n := atomic.AddInt64(&handler.stdioNotificationCount, 1)
		prefix += fmt.Sprintf("%s notif%d ", req.Method, n)
	} else {
		prefix += fmt.Sprintf("%s %v ", req.Method, req.ID)
	}

	params, err := lsp.ReadParams(req.Method, req.Params)
	if err != nil {
		return nil, err
	}
	if params == nil {
		params = req.Params
	}

	// Set up RWLocks and wait for clangd startup
	switch req.Method {
	case // Write lock - NO clangd required
		"initialize":
		handler.dataLock(prefix)
		defer handler.dataUnlock(prefix)
	case // Write lock - clangd required
		"textDocument/didOpen",
		"textDocument/didChange",
		"textDocument/didClose":
		handler.dataLock(prefix)
		defer handler.dataUnlock(prefix)
		handler.waitClangdStart(prefix)
	case // Read lock - NO clangd required
		"initialized":
		handler.dataRLock(prefix)
		defer handler.dataRUnlock(prefix)
	default: // Read lock - clangd required
		handler.dataRLock(prefix)
		// if clangd is not started...
		if handler.ClangdConn == nil {
			// Release the read lock and acquire a write lock
			// (this is required to wait on condition variable).
			handler.dataRUnlock(prefix)
			handler.dataLock(prefix)
			defer handler.dataUnlock(prefix)
			handler.waitClangdStart(prefix)
		} else {
			defer handler.dataRUnlock(prefix)
		}
	}

	// Handle LSP methods: transform parameters and send to clangd
	var inoURI, cppURI lsp.DocumentURI

	switch p := params.(type) {
	case *lsp.InitializeParams:
		// method "initialize"

		go func() {
			defer streams.CatchAndLogPanic()
			prefix := "INIT--- "
			log.Printf(prefix + "initializing workbench")

			// Start clangd asynchronously
			handler.dataLock(prefix)
			defer handler.dataUnlock(prefix)

			handler.initializeWorkbench(ctx, p)

			// clangd should be running now...
			handler.clangdStarted.Broadcast()

			log.Printf(prefix + "initializing workbench (done)")
		}()

		T := true
		F := false
		return &lsp.InitializeResult{
			Capabilities: lsp.ServerCapabilities{
				TextDocumentSync: &lsp.TextDocumentSyncOptionsOrKind{Kind: &lsp.TDSKIncremental},
				HoverProvider:    true,
				CompletionProvider: &lsp.CompletionOptions{
					TriggerCharacters: []string{".", "\u003e", ":"},
				},
				SignatureHelpProvider: &lsp.SignatureHelpOptions{
					TriggerCharacters: []string{"(", ","},
				},
				DefinitionProvider:              true,
				ReferencesProvider:              false, // TODO: true
				DocumentHighlightProvider:       true,
				DocumentSymbolProvider:          true,
				WorkspaceSymbolProvider:         true,
				CodeActionProvider:              &lsp.BoolOrCodeActionOptions{IsProvider: &T},
				DocumentFormattingProvider:      true,
				DocumentRangeFormattingProvider: true,
				DocumentOnTypeFormattingProvider: &lsp.DocumentOnTypeFormattingOptions{
					FirstTriggerCharacter: "\n",
				},
				RenameProvider: &lsp.BoolOrRenameOptions{IsProvider: &F}, // TODO: &T
				ExecuteCommandProvider: &lsp.ExecuteCommandOptions{
					Commands: []string{"clangd.applyFix", "clangd.applyTweak"},
				},
			},
		}, nil

	case *lsp.InitializedParams:
		// method "initialized"
		log.Println(prefix + "notification is not propagated to clangd")
		return nil, nil // Do not propagate to clangd

	case *lsp.DidOpenTextDocumentParams:
		// method "textDocument/didOpen"
		inoURI = p.TextDocument.URI
		log.Printf(prefix+"(%s@%d as '%s')", p.TextDocument.URI, p.TextDocument.Version, p.TextDocument.LanguageID)

		if res, e := handler.didOpen(p); e != nil {
			params = nil
			err = e
		} else if res == nil {
			log.Println(prefix + "notification is not propagated to clangd")
			return nil, nil // do not propagate to clangd
		} else {
			log.Printf(prefix+"to clang: didOpen(%s@%d as '%s')", res.TextDocument.URI, res.TextDocument.Version, res.TextDocument.LanguageID)
			params = res
		}

	case *lsp.DidCloseTextDocumentParams:
		// Method: "textDocument/didClose"
		inoURI = p.TextDocument.URI
		log.Printf("--> didClose(%s)", p.TextDocument.URI)

		if res, e := handler.didClose(p); e != nil {
		} else if res == nil {
			log.Println("    --X notification is not propagated to clangd")
			return nil, nil // do not propagate to clangd
		} else {
			log.Printf("    --> didClose(%s)", res.TextDocument.URI)
			params = res
		}

	case *lsp.DidChangeTextDocumentParams:
		// notification "textDocument/didChange"
		inoURI = p.TextDocument.URI
		log.Printf("--> didChange(%s@%d)", p.TextDocument.URI, p.TextDocument.Version)
		for _, change := range p.ContentChanges {
			log.Printf("     > %s -> %s", change.Range, strconv.Quote(change.Text))
		}

		if res, err := handler.didChange(ctx, p); err != nil {
			log.Printf("    --E error: %s", err)
			return nil, err
		} else if res == nil {
			log.Println("    --X notification is not propagated to clangd")
			return nil, err // do not propagate to clangd
		} else {
			p = res
		}

		log.Printf("    --> didChange(%s@%d)", p.TextDocument.URI, p.TextDocument.Version)
		for _, change := range p.ContentChanges {
			log.Printf("         > %s -> %s", change.Range, strconv.Quote(change.Text))
		}
		err = handler.ClangdConn.Notify(ctx, req.Method, p)
		return nil, err

	case *lsp.CompletionParams:
		// method: "textDocument/completion"
		log.Printf("--> completion(%s:%d:%d)\n", p.TextDocument.URI, p.Position.Line, p.Position.Character)

		if res, e := handler.ino2cppTextDocumentPositionParams(&p.TextDocumentPositionParams); e == nil {
			p.TextDocumentPositionParams = *res
			log.Printf("    --> completion(%s:%d:%d)\n", p.TextDocument.URI, p.Position.Line, p.Position.Character)
		} else {
			err = e
		}
		inoURI = p.TextDocument.URI

	case *lsp.CodeActionParams:
		// method "textDocument/codeAction"
		inoURI = p.TextDocument.URI
		log.Printf("--> codeAction(%s:%s)", p.TextDocument.URI, p.Range.Start)

		p.TextDocument, err = handler.ino2cppTextDocumentIdentifier(p.TextDocument)
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
		log.Printf("    --> codeAction(%s:%s)", p.TextDocument.URI, p.Range.Start)

	case *lsp.HoverParams:
		// method: "textDocument/hover"
		doc := &p.TextDocumentPositionParams
		log.Printf("--> hover(%s:%d:%d)\n", doc.TextDocument.URI, doc.Position.Line, doc.Position.Character)

		if res, e := handler.ino2cppTextDocumentPositionParams(doc); e == nil {
			p.TextDocumentPositionParams = *res
			log.Printf("    --> hover(%s:%d:%d)\n", doc.TextDocument.URI, doc.Position.Line, doc.Position.Character)
		} else {
			err = e
		}
		inoURI = p.TextDocument.URI

	case *lsp.DocumentSymbolParams:
		// method "textDocument/documentSymbol"
		inoURI = p.TextDocument.URI
		log.Printf("--> documentSymbol(%s)", p.TextDocument.URI)

		p.TextDocument, err = handler.ino2cppTextDocumentIdentifier(p.TextDocument)
		log.Printf("    --> documentSymbol(%s)", p.TextDocument.URI)

	case *lsp.DocumentFormattingParams:
		// method "textDocument/formatting"
		inoURI = p.TextDocument.URI
		log.Printf("--> formatting(%s)", p.TextDocument.URI)
		p.TextDocument, err = handler.ino2cppTextDocumentIdentifier(p.TextDocument)
		cppURI = p.TextDocument.URI
		log.Printf("    --> formatting(%s)", p.TextDocument.URI)
		if cleanup, e := handler.createClangdFormatterConfig(cppURI); e != nil {
			err = e
		} else {
			defer cleanup()
		}

	case *lsp.DocumentRangeFormattingParams:
		// Method: "textDocument/rangeFormatting"
		log.Printf("--> %s(%s:%s)", req.Method, p.TextDocument.URI, p.Range)
		inoURI = p.TextDocument.URI
		if cppParams, e := handler.ino2cppDocumentRangeFormattingParams(p); e == nil {
			params = cppParams
			cppURI = cppParams.TextDocument.URI
			log.Printf("    --> %s(%s:%s)", req.Method, cppParams.TextDocument.URI, cppParams.Range)
			if cleanup, e := handler.createClangdFormatterConfig(cppURI); e != nil {
				err = e
			} else {
				defer cleanup()
			}
		} else {
			err = e
		}

	case *lsp.TextDocumentPositionParams:
		// Method: "textDocument/signatureHelp"
		// Method: "textDocument/definition"
		// Method: "textDocument/typeDefinition"
		// Method: "textDocument/implementation"
		// Method: "textDocument/documentHighlight"
		log.Printf("--> %s(%s:%s)", req.Method, p.TextDocument.URI, p.Position)
		inoURI = p.TextDocument.URI
		if res, e := handler.ino2cppTextDocumentPositionParams(p); e == nil {
			cppURI = res.TextDocument.URI
			params = res
			log.Printf("    --> %s(%s:%s)", req.Method, res.TextDocument.URI, res.Position)
		} else {
			err = e
		}

	case *lsp.DidSaveTextDocumentParams:
		// Method: "textDocument/didSave"
		log.Printf("--> %s(%s)", req.Method, p.TextDocument.URI)
		inoURI = p.TextDocument.URI
		p.TextDocument, err = handler.ino2cppTextDocumentIdentifier(p.TextDocument)
		cppURI = p.TextDocument.URI
		if cppURI.AsPath().EquivalentTo(handler.buildSketchCpp) {
			log.Printf("    --| didSave not forwarded to clangd")
			return nil, nil
		}
		log.Printf("    --> %s(%s)", req.Method, p.TextDocument.URI)

	case *lsp.ReferenceParams: // "textDocument/references":
		log.Printf("--X " + req.Method)
		return nil, nil
		inoURI = p.TextDocument.URI
		_, err = handler.ino2cppTextDocumentPositionParams(&p.TextDocumentPositionParams)
	case *lsp.DocumentOnTypeFormattingParams: // "textDocument/onTypeFormatting":
		log.Printf("--X " + req.Method)
		return nil, nil
		inoURI = p.TextDocument.URI
		err = handler.ino2cppDocumentOnTypeFormattingParams(p)
	case *lsp.RenameParams: // "textDocument/rename":
		log.Printf("--X " + req.Method)
		return nil, nil
		inoURI = p.TextDocument.URI
		err = handler.ino2cppRenameParams(p)
	case *lsp.DidChangeWatchedFilesParams: // "workspace/didChangeWatchedFiles":
		log.Printf("--X " + req.Method)
		return nil, nil
		err = handler.ino2cppDidChangeWatchedFilesParams(p)
	case *lsp.ExecuteCommandParams: // "workspace/executeCommand":
		log.Printf("--X " + req.Method)
		return nil, nil
		err = handler.ino2cppExecuteCommand(p)
	}
	if err != nil {
		log.Printf(prefix+"Error: %s", err)
		return nil, err
	}

	var result interface{}
	if req.Notif {
		log.Printf(prefix + "sent to Clang")
		err = handler.ClangdConn.Notify(ctx, req.Method, params)
	} else {
		log.Printf(prefix + "sent to Clang")
		result, err = lsp.SendRequest(ctx, handler.ClangdConn, req.Method, params)
	}
	if err == nil && handler.buildSketchSymbolsLoad {
		handler.buildSketchSymbolsLoad = false
		handler.buildSketchSymbolsCheck = false
		log.Println(prefix + "Queued resfreshing document symbols")
		go handler.LoadCppDocumentSymbols()
	}
	if err == nil && handler.buildSketchSymbolsCheck {
		handler.buildSketchSymbolsCheck = false
		log.Println(prefix + "Queued check document symbols")
		go handler.CheckCppDocumentSymbols()
	}
	if err != nil {
		// Exit the process and trigger a restart by the client in case of a severe error
		if err.Error() == "context deadline exceeded" {
			log.Println(prefix + "Timeout exceeded while waiting for a reply from clangd.")
			log.Println(prefix + "Please restart the language server.")
			handler.Close()
		}
		if strings.Contains(err.Error(), "non-added document") || strings.Contains(err.Error(), "non-added file") {
			log.Printf(prefix + "The clangd process has lost track of the open document.")
			log.Printf(prefix+"  %s", err)
			log.Println(prefix + "Please restart the language server.")
			handler.Close()
		}
	}

	// Transform and return the result
	if result != nil {
		result = handler.transformClangdResult(req.Method, inoURI, cppURI, result)
	}
	return result, err
}

func (handler *InoHandler) initializeWorkbench(ctx context.Context, params *lsp.InitializeParams) error {
	currCppTextVersion := 0
	if params != nil {
		log.Printf("    --> initialize(%s)\n", params.RootURI)
		handler.lspInitializeParams = params
		handler.sketchRoot = params.RootURI.AsPath()
		handler.sketchName = handler.sketchRoot.Base()
	} else {
		log.Printf("    --> RE-initialize()\n")
		currCppTextVersion = handler.sketchMapper.CppText.Version
	}

	if err := handler.generateBuildEnvironment(handler.buildPath); err != nil {
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

		if err := handler.ClangdConn.Notify(ctx, "textDocument/didChange", syncEvent); err != nil {
			log.Println("    error reinitilizing clangd:", err)
			return err
		}
	} else {
		// Otherwise start clangd!
		dataFolder, err := extractDataFolderFromArduinoCLI()
		if err != nil {
			log.Printf("    error: %s", err)
		}
		clangdStdout, clangdStdin, clangdStderr := startClangd(handler.buildPath, handler.buildSketchCpp, dataFolder)
		clangdStdio := streams.NewReadWriteCloser(clangdStdin, clangdStdout)
		if enableLogging {
			clangdStdio = streams.LogReadWriteCloserAs(clangdStdio, "inols-clangd.log")
			go io.Copy(streams.OpenLogFileAs("inols-clangd-err.log"), clangdStderr)
		} else {
			go io.Copy(os.Stderr, clangdStderr)
		}

		clangdStream := jsonrpc2.NewBufferedStream(clangdStdio, jsonrpc2.VSCodeObjectCodec{})
		clangdHandler := AsyncHandler{jsonrpc2.HandlerWithError(handler.FromClangd)}
		handler.ClangdConn = jsonrpc2.NewConn(context.Background(), clangdStream, clangdHandler,
			jsonrpc2.OnRecv(streams.JSONRPCConnLogOnRecv("IDE     LS <-- CL:")),
			jsonrpc2.OnSend(streams.JSONRPCConnLogOnSend("IDE     LS --> CL:")))
		go func() {
			<-handler.ClangdConn.DisconnectNotify()
			log.Printf("Lost connection with clangd!")
			handler.Close()
		}()

		// Send initialization command to clangd
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var resp lsp.InitializeResult
		if err := handler.ClangdConn.Call(ctx, "initialize", handler.lspInitializeParams, &resp); err != nil {
			log.Println("    error initilizing clangd:", err)
			return err
		}

		if err := handler.ClangdConn.Notify(ctx, "initialized", lsp.InitializedParams{}); err != nil {
			log.Println("    error sending initialize to clangd:", err)
			return err
		}
	}

	handler.buildSketchSymbolsLoad = true
	return nil
}

func extractDataFolderFromArduinoCLI() (*paths.Path, error) {
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
	log.Println("running: ", strings.Join(args, " "))
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
	log.Println("Arduino Data Dir -> ", res.Directories.Data)
	return paths.New(res.Directories.Data), nil
}

func (handler *InoHandler) refreshCppDocumentSymbols(prefix string) error {
	// Query source code symbols
	cppURI := lsp.NewDocumentURIFromPath(handler.buildSketchCpp)
	log.Printf(prefix+"requesting documentSymbol for %s", cppURI)

	handler.dataRUnlock(prefix)
	result, err := lsp.SendRequest(context.Background(), handler.ClangdConn, "textDocument/documentSymbol", &lsp.DocumentSymbolParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: cppURI},
	})
	handler.dataRLock(prefix)

	if err != nil {
		log.Printf(prefix+"error: %s", err)
		return errors.WithMessage(err, "quering source code symbols")
	}

	symbolResult, ok := result.(*lsp.DocumentSymbolArrayOrSymbolInformationArray)
	if !ok || symbolResult.DocumentSymbolArray == nil {
		log.Printf(prefix + "error: expected DocumenSymbol array from clangd")
		return errors.New("expected array from clangd")
	}
	symbols := *symbolResult.DocumentSymbolArray

	// Filter non-functions symbols
	i := 0
	for _, symbol := range symbols {
		if symbol.Kind != lsp.SKFunction {
			continue
		}
		symbols[i] = symbol
		i++
	}
	symbols = symbols[:i]
	handler.buildSketchSymbols = symbols

	symbolsCanary := ""
	for _, symbol := range symbols {
		log.Printf(prefix+"   symbol: %s %s %s", symbol.Kind, symbol.Name, symbol.Range)
		if symbolText, err := textutils.ExtractRange(handler.sketchMapper.CppText.Text, symbol.Range); err != nil {
			log.Printf(prefix+"     > invalid range: %s", err)
			symbolsCanary += "/"
		} else if end := strings.Index(symbolText, "{"); end != -1 {
			log.Printf(prefix+"     TRIMMED> %s", symbolText[:end])
			symbolsCanary += symbolText[:end]
		} else {
			log.Printf(prefix+"            > %s", symbolText)
			symbolsCanary += symbolText
		}
	}
	handler.buildSketchSymbolsCanary = symbolsCanary
	return nil
}

func (handler *InoHandler) LoadCppDocumentSymbols() error {
	prefix := "SYLD--- "
	defer log.Printf(prefix + "(done)")
	handler.dataRLock(prefix)
	defer handler.dataRUnlock(prefix)
	return handler.refreshCppDocumentSymbols(prefix)
}

func (handler *InoHandler) CheckCppDocumentSymbols() error {
	prefix := "SYCK--- "
	defer log.Printf(prefix + "(done)")
	handler.dataRLock(prefix)
	defer handler.dataRUnlock(prefix)

	oldSymbols := handler.buildSketchSymbols
	canary := handler.buildSketchSymbolsCanary
	if err := handler.refreshCppDocumentSymbols(prefix); err != nil {
		return err
	}
	if len(oldSymbols) != len(handler.buildSketchSymbols) || canary != handler.buildSketchSymbolsCanary {
		log.Println(prefix + "function symbols change detected, triggering sketch rebuild!")
		handler.scheduleRebuildEnvironment()
	}
	return nil
}

func (handler *InoHandler) CheckCppIncludesChanges() {
	prefix := "INCK--- "

	includesCanary := ""
	for _, line := range strings.Split(handler.sketchMapper.CppText.Text, "\n") {
		if strings.Contains(line, "#include ") {
			includesCanary += line
		}
	}

	if includesCanary != handler.buildSketchIncludesCanary {
		handler.buildSketchIncludesCanary = includesCanary
		log.Println(prefix + "#include change detected, triggering sketch rebuild!")
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

func startClangd(compileCommandsDir, sketchCpp *paths.Path, dataFolder *paths.Path) (io.WriteCloser, io.ReadCloser, io.ReadCloser) {
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

func (handler *InoHandler) didOpen(inoDidOpen *lsp.DidOpenTextDocumentParams) (*lsp.DidOpenTextDocumentParams, error) {
	// Add the TextDocumentItem in the tracked files list
	inoItem := inoDidOpen.TextDocument
	handler.docs[inoItem.URI.AsPath().String()] = &inoItem

	// If we are tracking a .ino...
	if inoItem.URI.Ext() == ".ino" {
		handler.sketchTrackedFilesCount++
		log.Printf("    increasing .ino tracked files count: %d", handler.sketchTrackedFilesCount)

		// notify clang that sketchCpp has been opened only once
		if handler.sketchTrackedFilesCount != 1 {
			return nil, nil
		}
	}

	cppItem, err := handler.ino2cppTextDocumentItem(inoItem)
	return &lsp.DidOpenTextDocumentParams{
		TextDocument: cppItem,
	}, err
}

func (handler *InoHandler) didClose(inoDidClose *lsp.DidCloseTextDocumentParams) (*lsp.DidCloseTextDocumentParams, error) {
	inoIdentifier := inoDidClose.TextDocument
	if _, exist := handler.docs[inoIdentifier.URI.AsPath().String()]; exist {
		delete(handler.docs, inoIdentifier.URI.AsPath().String())
	} else {
		log.Printf("    didClose of untracked document: %s", inoIdentifier.URI)
		return nil, unknownURI(inoIdentifier.URI)
	}

	// If we are tracking a .ino...
	if inoIdentifier.URI.Ext() == ".ino" {
		handler.sketchTrackedFilesCount--
		log.Printf("    decreasing .ino tracked files count: %d", handler.sketchTrackedFilesCount)

		// notify clang that sketchCpp has been close only once all .ino are closed
		if handler.sketchTrackedFilesCount != 0 {
			return nil, nil
		}
	}

	cppIdentifier, err := handler.ino2cppTextDocumentIdentifier(inoIdentifier)
	return &lsp.DidCloseTextDocumentParams{
		TextDocument: cppIdentifier,
	}, err
}

func (handler *InoHandler) ino2cppTextDocumentItem(inoItem lsp.TextDocumentItem) (cppItem lsp.TextDocumentItem, err error) {
	cppURI, err := handler.ino2cppDocumentURI(inoItem.URI)
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

func (handler *InoHandler) didChange(ctx context.Context, req *lsp.DidChangeTextDocumentParams) (*lsp.DidChangeTextDocumentParams, error) {
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
			cppRange, ok := handler.sketchMapper.InoToCppLSPRangeOk(doc.URI, *inoChange.Range)
			if !ok {
				return nil, errors.Errorf("invalid change range %s:%s", doc.URI, *inoChange.Range)
			}

			// Detect changes in critical lines (for example function definitions)
			// and trigger arduino-preprocessing + clangd restart.
			dirty := false
			for _, sym := range handler.buildSketchSymbols {
				if sym.SelectionRange.Overlaps(cppRange) {
					dirty = true
					log.Println("--! DIRTY CHANGE detected using symbol tables, force sketch rebuild!")
					break
				}
			}
			if handler.sketchMapper.ApplyTextChange(doc.URI, inoChange) {
				dirty = true
				log.Println("--! DIRTY CHANGE detected with sketch mapper, force sketch rebuild!")
			}
			if dirty {
				handler.scheduleRebuildEnvironment()
			}

			// log.Println("New version:----------")
			// log.Println(handler.sketchMapper.CppText.Text)
			// log.Println("----------------------")

			cppChange := lsp.TextDocumentContentChangeEvent{
				Range:       &cppRange,
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
	cppDoc, err := handler.ino2cppVersionedTextDocumentIdentifier(req.TextDocument)
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
	go handler.showMessage(ctx, lsp.MTError, message)
	return errors.New(message)
}

func (handler *InoHandler) ino2cppVersionedTextDocumentIdentifier(doc lsp.VersionedTextDocumentIdentifier) (lsp.VersionedTextDocumentIdentifier, error) {
	cppURI, err := handler.ino2cppDocumentURI(doc.URI)
	res := doc
	res.URI = cppURI
	return res, err
}

func (handler *InoHandler) ino2cppTextDocumentIdentifier(doc lsp.TextDocumentIdentifier) (lsp.TextDocumentIdentifier, error) {
	cppURI, err := handler.ino2cppDocumentURI(doc.URI)
	res := doc
	res.URI = cppURI
	return res, err
}

func (handler *InoHandler) ino2cppDocumentURI(inoURI lsp.DocumentURI) (lsp.DocumentURI, error) {
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
		log.Printf("    could not determine if '%s' is inside '%s'", inoPath, handler.sketchRoot)
		return lsp.NilURI, unknownURI(inoURI)
	}
	if !inside {
		log.Printf("    '%s' not inside sketchroot '%s', passing doc identifier to as-is", handler.sketchRoot, inoPath)
		return inoURI, nil
	}

	rel, err := handler.sketchRoot.RelTo(inoPath)
	if err == nil {
		cppPath := handler.buildSketchRoot.JoinPath(rel)
		log.Printf("    URI: '%s' -> '%s'", inoPath, cppPath)
		return lsp.NewDocumentURIFromPath(cppPath), nil
	}

	log.Printf("    could not determine rel-path of '%s' in '%s': %s", inoPath, handler.sketchRoot, err)
	return lsp.NilURI, err
}

func (handler *InoHandler) inoDocumentURIFromInoPath(inoPath string) (lsp.DocumentURI, error) {
	if inoPath == sourcemapper.NotIno.File {
		return sourcemapper.NotInoURI, nil
	}
	doc, ok := handler.docs[inoPath]
	if !ok {
		log.Printf("    !!! Unresolved .ino path: %s", inoPath)
		log.Printf("    !!! Known doc paths are:")
		for p := range handler.docs {
			log.Printf("    !!! > %s", p)
		}
		uri := lsp.NewDocumentURI(inoPath)
		return uri, unknownURI(uri)
	}
	return doc.URI, nil
}

func (handler *InoHandler) cpp2inoDocumentURI(cppURI lsp.DocumentURI, cppRange lsp.Range) (lsp.DocumentURI, lsp.Range, error) {
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
				log.Printf("    URI: is in preprocessed section")
				log.Printf("         converted %s to %s:%s", cppRange, inoPath, inoRange)
			} else {
				log.Printf("    URI: converted %s to %s:%s", cppRange, inoPath, inoRange)
			}
		} else if _, ok := err.(sourcemapper.AdjustedRangeErr); ok {
			log.Printf("    URI: converted %s to %s:%s (END LINE ADJUSTED)", cppRange, inoPath, inoRange)
			err = nil
		} else {
			log.Printf("    URI: ERROR: %s", err)
			handler.sketchMapper.DebugLogAll()
			return lsp.NilURI, lsp.NilRange, err
		}
		inoURI, err := handler.inoDocumentURIFromInoPath(inoPath)
		return inoURI, inoRange, err
	}

	inside, err := cppPath.IsInsideDir(handler.buildSketchRoot)
	if err != nil {
		log.Printf("    could not determine if '%s' is inside '%s'", cppPath, handler.buildSketchRoot)
		return lsp.NilURI, lsp.NilRange, err
	}
	if !inside {
		log.Printf("    '%s' is not inside '%s'", cppPath, handler.buildSketchRoot)
		log.Printf("    keep doc identifier to '%s' as-is", cppPath)
		return cppURI, cppRange, nil
	}

	rel, err := handler.buildSketchRoot.RelTo(cppPath)
	if err == nil {
		inoPath := handler.sketchRoot.JoinPath(rel).String()
		log.Printf("    URI: '%s' -> '%s'", cppPath, inoPath)
		inoURI, err := handler.inoDocumentURIFromInoPath(inoPath)
		log.Printf("              as URI: '%s'", inoURI)
		return inoURI, cppRange, err
	}

	log.Printf("    could not determine rel-path of '%s' in '%s': %s", cppPath, handler.buildSketchRoot, err)
	return lsp.NilURI, lsp.NilRange, err
}

func (handler *InoHandler) ino2cppTextDocumentPositionParams(inoParams *lsp.TextDocumentPositionParams) (*lsp.TextDocumentPositionParams, error) {
	cppDoc, err := handler.ino2cppTextDocumentIdentifier(inoParams.TextDocument)
	if err != nil {
		return nil, err
	}
	cppPosition := inoParams.Position
	inoURI := inoParams.TextDocument.URI
	if inoURI.Ext() == ".ino" {
		if cppLine, ok := handler.sketchMapper.InoToCppLineOk(inoURI, inoParams.Position.Line); ok {
			cppPosition.Line = cppLine
		} else {
			log.Printf("    invalid line requested: %s:%d", inoURI, inoParams.Position.Line)
			return nil, unknownURI(inoURI)
		}
	}
	return &lsp.TextDocumentPositionParams{
		TextDocument: cppDoc,
		Position:     cppPosition,
	}, nil
}

func (handler *InoHandler) ino2cppRange(inoURI lsp.DocumentURI, inoRange lsp.Range) (lsp.DocumentURI, lsp.Range, error) {
	cppURI, err := handler.ino2cppDocumentURI(inoURI)
	if err != nil {
		return lsp.NilURI, lsp.Range{}, err
	}
	if cppURI.AsPath().EquivalentTo(handler.buildSketchCpp) {
		cppRange := handler.sketchMapper.InoToCppLSPRange(inoURI, inoRange)
		return cppURI, cppRange, nil
	}
	return cppURI, inoRange, nil
}

func (handler *InoHandler) ino2cppDocumentRangeFormattingParams(inoParams *lsp.DocumentRangeFormattingParams) (*lsp.DocumentRangeFormattingParams, error) {
	cppTextDocument, err := handler.ino2cppTextDocumentIdentifier(inoParams.TextDocument)
	if err != nil {
		return nil, err
	}

	_, cppRange, err := handler.ino2cppRange(inoParams.TextDocument.URI, inoParams.Range)
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
	newEdit := lsp.WorkspaceEdit{Changes: make(map[lsp.DocumentURI][]lsp.TextEdit)}
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

func (handler *InoHandler) transformClangdResult(method string, inoURI, cppURI lsp.DocumentURI, result interface{}) interface{} {
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
		log.Printf("<-- hover(%s)", strconv.Quote(r.Contents.Value))
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
		log.Printf("<-- completion(%d items) cppToIno=%v", len(r.Items), cppToIno)
		return r

	case *lsp.DocumentSymbolArrayOrSymbolInformationArray:
		// method "textDocument/documentSymbol"

		if r.DocumentSymbolArray != nil {
			// Treat the input as []DocumentSymbol
			log.Printf("    <-- documentSymbol(%d document symbols)", len(*r.DocumentSymbolArray))
			return handler.cpp2inoDocumentSymbols(*r.DocumentSymbolArray, inoURI)
		} else if r.SymbolInformationArray != nil {
			// Treat the input as []SymbolInformation
			log.Printf("    <-- documentSymbol(%d symbol information)", len(*r.SymbolInformationArray))
			return handler.cpp2inoSymbolInformation(*r.SymbolInformationArray)
		} else {
			// Treat the input as null
			log.Printf("    <-- null documentSymbol")
		}

	case *[]lsp.CommandOrCodeAction:
		// method "textDocument/codeAction"
		log.Printf("    <-- codeAction(%d elements)", len(*r))
		for i, item := range *r {
			if item.Command != nil {
				log.Printf("        > Command: %s", item.Command.Title)
			}
			if item.CodeAction != nil {
				log.Printf("        > CodeAction: %s", item.CodeAction.Title)
			}
			(*r)[i] = lsp.CommandOrCodeAction{
				Command:    handler.Cpp2InoCommand(item.Command),
				CodeAction: handler.cpp2inoCodeAction(item.CodeAction, inoURI),
			}
		}
		log.Printf("<-- codeAction(%d elements)", len(*r))

	case *[]lsp.TextEdit:
		// Method: "textDocument/rangeFormatting"
		// Method: "textDocument/onTypeFormatting"
		// Method: "textDocument/formatting"
		log.Printf("    <-- %s %s textEdit(%d elements)", method, cppURI, len(*r))
		for _, edit := range *r {
			log.Printf("        > %s -> %s", edit.Range, strconv.Quote(edit.NewText))
		}
		sketchEdits, err := handler.cpp2inoTextEdits(cppURI, *r)
		if err != nil {
			log.Printf("ERROR converting textEdits: %s", err)
			return nil
		}

		inoEdits, ok := sketchEdits[inoURI]
		if !ok {
			inoEdits = []lsp.TextEdit{}
		}
		log.Printf("<-- %s %s textEdit(%d elements)", method, inoURI, len(inoEdits))
		for _, edit := range inoEdits {
			log.Printf("        > %s -> %s", edit.Range, strconv.Quote(edit.NewText))
		}
		return &inoEdits

	case *[]lsp.Location:
		// Method: "textDocument/definition"
		// Method: "textDocument/typeDefinition"
		// Method: "textDocument/implementation"
		// Method: "textDocument/references"
		inoLocations := []lsp.Location{}
		for _, cppLocation := range *r {
			inoLocation, err := handler.cpp2inoLocation(cppLocation)
			if err != nil {
				log.Printf("ERROR converting location %s:%s: %s", cppLocation.URI, cppLocation.Range, err)
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
			inoLocation, err := handler.cpp2inoLocation(cppLocation)
			if err != nil {
				log.Printf("ERROR converting location %s:%s: %s", cppLocation.URI, cppLocation.Range, err)
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
			inoHL, err := handler.cpp2inoDocumentHighlight(&cppHL, cppURI)
			if err != nil {
				log.Printf("ERROR converting location %s:%s: %s", cppURI, cppHL.Range, err)
				return nil
			}
			res = append(res, *inoHL)
		}
		return &res

	case *lsp.WorkspaceEdit: // "textDocument/rename":
		return handler.cpp2inoWorkspaceEdit(r)
	}
	return result
}

func (handler *InoHandler) cpp2inoCodeAction(codeAction *lsp.CodeAction, uri lsp.DocumentURI) *lsp.CodeAction {
	if codeAction == nil {
		return nil
	}
	inoCodeAction := &lsp.CodeAction{
		Title:       codeAction.Title,
		Kind:        codeAction.Kind,
		Edit:        handler.cpp2inoWorkspaceEdit(codeAction.Edit),
		Diagnostics: codeAction.Diagnostics,
		Command:     handler.Cpp2InoCommand(codeAction.Command),
	}
	if uri.Ext() == ".ino" {
		for i, diag := range inoCodeAction.Diagnostics {
			_, inoCodeAction.Diagnostics[i].Range = handler.sketchMapper.CppToInoRange(diag.Range)
		}
	}
	return inoCodeAction
}

func (handler *InoHandler) Cpp2InoCommand(command *lsp.Command) *lsp.Command {
	if command == nil {
		return nil
	}
	inoCommand := &lsp.Command{
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
					log.Println("            > converted clangd ExtractVariable")
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

func (handler *InoHandler) cpp2inoWorkspaceEdit(cppWorkspaceEdit *lsp.WorkspaceEdit) *lsp.WorkspaceEdit {
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
			inoURI, inoRange, err := handler.cpp2inoDocumentURI(editURI, edit.Range)
			if err != nil {
				log.Printf("    error converting edit %s:%s: %s", editURI, edit.Range, err)
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
	log.Printf("    done converting workspaceEdit")
	return inoWorkspaceEdit
}

func (handler *InoHandler) cpp2inoLocation(cppLocation lsp.Location) (lsp.Location, error) {
	inoURI, inoRange, err := handler.cpp2inoDocumentURI(cppLocation.URI, cppLocation.Range)
	return lsp.Location{
		URI:   inoURI,
		Range: inoRange,
	}, err
}

func (handler *InoHandler) cpp2inoDocumentHighlight(cppHighlight *lsp.DocumentHighlight, cppURI lsp.DocumentURI) (*lsp.DocumentHighlight, error) {
	_, inoRange, err := handler.cpp2inoDocumentURI(cppURI, cppHighlight.Range)
	if err != nil {
		return nil, err
	}
	return &lsp.DocumentHighlight{
		Kind:  cppHighlight.Kind,
		Range: inoRange,
	}, nil
}

func (handler *InoHandler) cpp2inoTextEdits(cppURI lsp.DocumentURI, cppEdits []lsp.TextEdit) (map[lsp.DocumentURI][]lsp.TextEdit, error) {
	res := map[lsp.DocumentURI][]lsp.TextEdit{}
	for _, cppEdit := range cppEdits {
		inoURI, inoEdit, err := handler.cpp2inoTextEdit(cppURI, cppEdit)
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

func (handler *InoHandler) cpp2inoTextEdit(cppURI lsp.DocumentURI, cppEdit lsp.TextEdit) (lsp.DocumentURI, lsp.TextEdit, error) {
	inoURI, inoRange, err := handler.cpp2inoDocumentURI(cppURI, cppEdit.Range)
	inoEdit := cppEdit
	inoEdit.Range = inoRange
	return inoURI, inoEdit, err
}

func (handler *InoHandler) cpp2inoDocumentSymbols(cppSymbols []lsp.DocumentSymbol, inoRequestedURI lsp.DocumentURI) []lsp.DocumentSymbol {
	inoRequested := inoRequestedURI.AsPath().String()
	log.Printf("    filtering for requested ino file: %s", inoRequested)
	if inoRequestedURI.Ext() != ".ino" || len(cppSymbols) == 0 {
		return cppSymbols
	}

	inoSymbols := []lsp.DocumentSymbol{}
	for _, symbol := range cppSymbols {
		log.Printf("    > convert %s %s", symbol.Kind, symbol.Range)
		if handler.sketchMapper.IsPreprocessedCppLine(symbol.Range.Start.Line) {
			log.Printf("      symbol is in the preprocessed section of the sketch.ino.cpp")
			continue
		}

		inoFile, inoRange := handler.sketchMapper.CppToInoRange(symbol.Range)
		inoSelectionURI, inoSelectionRange := handler.sketchMapper.CppToInoRange(symbol.SelectionRange)

		if inoFile != inoSelectionURI {
			log.Printf("      ERROR: symbol range and selection belongs to different URI!")
			log.Printf("        symbol %s != selection %s", symbol.Range, symbol.SelectionRange)
			log.Printf("        %s:%s != %s:%s", inoFile, inoRange, inoSelectionURI, inoSelectionRange)
			continue
		}

		if inoFile != inoRequested {
			log.Printf("    skipping symbol related to %s", inoFile)
			continue
		}

		inoSymbols = append(inoSymbols, lsp.DocumentSymbol{
			Name:           symbol.Name,
			Detail:         symbol.Detail,
			Deprecated:     symbol.Deprecated,
			Kind:           symbol.Kind,
			Range:          inoRange,
			SelectionRange: inoSelectionRange,
			Children:       handler.cpp2inoDocumentSymbols(symbol.Children, inoRequestedURI),
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

func (handler *InoHandler) cpp2inoDiagnostics(cppDiags *lsp.PublishDiagnosticsParams) ([]*lsp.PublishDiagnosticsParams, error) {
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
		inoURI, _, err := handler.cpp2inoDocumentURI(cppURI, lsp.NilRange)
		if err != nil {
			return nil, err
		}
		inoDiagsParam[inoURI] = &lsp.PublishDiagnosticsParams{
			URI:         inoURI,
			Diagnostics: []lsp.Diagnostic{},
		}
	}

	for _, cppDiag := range cppDiags.Diagnostics {
		inoURI, inoRange, err := handler.cpp2inoDocumentURI(cppURI, cppDiag.Range)
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
			if inoDiag.Code == "undeclared_var_use_suggest" ||
				inoDiag.Code == "undeclared_var_use" ||
				inoDiag.Code == "ovl_no_viable_function_in_call" ||
				inoDiag.Code == "pp_file_not_found" {
				handler.buildSketchSymbolsCheck = true
			}
		}
	}

	inoDiagParams := []*lsp.PublishDiagnosticsParams{}
	for _, v := range inoDiagsParam {
		inoDiagParams = append(inoDiagParams, v)
	}
	return inoDiagParams, nil
}

// FromClangd handles a message received from clangd.
func (handler *InoHandler) FromClangd(ctx context.Context, connection *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error) {
	defer streams.CatchAndLogPanic()

	prefix := "CLG <-- "
	if req.Notif {
		n := atomic.AddInt64(&handler.clangdNotificationCount, 1)
		prefix += fmt.Sprintf("%s notif%d ", req.Method, n)
	} else {
		prefix += fmt.Sprintf("%s %v ", req.Method, req.ID)
	}

	if req.Method == "window/workDoneProgress/create" {
		params := lsp.WorkDoneProgressCreateParams{}
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			log.Printf(prefix+"error decoding window/workDoneProgress/create: %v", err)
			return nil, err
		}
		handler.progressHandler.Create(params.Token)
		return &lsp.WorkDoneProgressCreateResult{}, nil
	}

	if req.Method == "$/progress" {
		// data may be of many different types...
		params := lsp.ProgressParams{}
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			log.Printf(prefix+"error decoding progress: %v", err)
			return nil, err
		}
		id := params.Token

		var begin lsp.WorkDoneProgressBegin
		if err := json.Unmarshal(*params.Value, &begin); err == nil {
			// log.Printf(prefix+"begin %s %v", id, begin)
			handler.progressHandler.Begin(id, &begin)
			return nil, nil
		}

		var report lsp.WorkDoneProgressReport
		if err := json.Unmarshal(*params.Value, &report); err == nil {
			// log.Printf(prefix+"report %s %v", id, report)
			handler.progressHandler.Report(id, &report)
			return nil, nil
		}

		var end lsp.WorkDoneProgressEnd
		if err := json.Unmarshal(*params.Value, &end); err == nil {
			// log.Printf(prefix+"end %s %v", id, end)
			handler.progressHandler.End(id, &end)
			return nil, nil
		}

		log.Printf(prefix + "error unsupported $/progress: " + string(*params.Value))
		return nil, errors.New("unsupported $/progress: " + string(*params.Value))
	}

	// Default to read lock
	log.Printf(prefix + "(queued)")
	handler.dataRLock(prefix)
	defer handler.dataRUnlock(prefix)
	log.Printf(prefix + "(running)")

	params, err := lsp.ReadParams(req.Method, req.Params)
	if err != nil {
		log.Println(prefix+"parsing clang message:", err)
		return nil, errors.WithMessage(err, "parsing JSON message from clangd")
	}

	switch p := params.(type) {
	case *lsp.PublishDiagnosticsParams:
		// "textDocument/publishDiagnostics"
		log.Printf(prefix+"publishDiagnostics(%s):", p.URI)
		for _, diag := range p.Diagnostics {
			log.Printf(prefix+"> %d:%d - %v: %s", diag.Range.Start.Line, diag.Range.Start.Character, diag.Severity, diag.Code)
		}

		// the diagnostics on sketch.cpp.ino once mapped into their
		// .ino counter parts may span over multiple .ino files...
		inoDiagnostics, err := handler.cpp2inoDiagnostics(p)
		if err != nil {
			return nil, err
		}

		// Push back to IDE the converted diagnostics
		for _, inoDiag := range inoDiagnostics {

			log.Printf(prefix+"to IDE: publishDiagnostics(%s):", inoDiag.URI)
			for _, diag := range inoDiag.Diagnostics {
				log.Printf(prefix+"> %d:%d - %v: %s", diag.Range.Start.Line, diag.Range.Start.Character, diag.Severity, diag.Code)
			}
			if err := handler.StdioConn.Notify(ctx, "textDocument/publishDiagnostics", inoDiag); err != nil {
				return nil, err
			}
		}
		return nil, err

	case *lsp.ApplyWorkspaceEditParams:
		// "workspace/applyEdit"
		p.Edit = *handler.cpp2inoWorkspaceEdit(&p.Edit)
	}
	if err != nil {
		log.Println("From clangd: Method:", req.Method, "Error:", err)
		return nil, err
	}

	if params == nil {
		// passthrough
		log.Printf(prefix + "passing through message")
		params = req.Params
	}

	var result interface{}
	if req.Notif {
		log.Println(prefix + "to IDE")
		err = handler.StdioConn.Notify(ctx, req.Method, params)
	} else {
		log.Println(prefix + "to IDE")
		result, err = lsp.SendRequest(ctx, handler.StdioConn, req.Method, params)
	}
	return result, err
}

func (handler *InoHandler) createClangdFormatterConfig(cppuri lsp.DocumentURI) (func(), error) {
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
			log.Printf("    error reading custom formatter config file %s: %s", conf, err)
		} else {
			log.Printf("    using custom formatter config file %s", conf)
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
		log.Printf("    formatter config cleaned")
	}
	log.Printf("    writing formatter config in: %s", targetFile)
	err := targetFile.WriteFile([]byte(config))
	return cleanup, err
}

func (handler *InoHandler) showMessage(ctx context.Context, msgType lsp.MessageType, message string) {
	defer streams.CatchAndLogPanic()

	params := lsp.ShowMessageParams{
		Type:    msgType,
		Message: message,
	}
	handler.StdioConn.Notify(ctx, "window/showMessage", &params)
}

func unknownURI(uri lsp.DocumentURI) error {
	return errors.New("Document is not available: " + uri.String())
}
