package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arduino/arduino-cli/arduino/builder"
	"github.com/arduino/arduino-cli/executils"
	"github.com/arduino/go-paths-helper"
	"github.com/bcmi-labs/arduino-language-server/handler/sourcemapper"
	"github.com/bcmi-labs/arduino-language-server/handler/textutils"
	"github.com/bcmi-labs/arduino-language-server/lsp"
	"github.com/bcmi-labs/arduino-language-server/streams"
	"github.com/pkg/errors"
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

// InoHandler is a JSON-RPC handler that delegates messages to clangd.
type InoHandler struct {
	StdioConn                  *jsonrpc2.Conn
	ClangdConn                 *jsonrpc2.Conn
	lspInitializeParams        *lsp.InitializeParams
	buildPath                  *paths.Path
	buildSketchRoot            *paths.Path
	buildSketchCpp             *paths.Path
	buildSketchCppVersion      int
	buildSketchSymbols         []lsp.DocumentSymbol
	buildSketchSymbolsLoad     bool
	buildSketchSymbolsCheck    bool
	rebuildSketchDeadline      *time.Time
	rebuildSketchDeadlineMutex sync.Mutex
	sketchRoot                 *paths.Path
	sketchName                 string
	sketchMapper               *sourcemapper.InoMapper
	sketchTrackedFilesCount    int
	docs                       map[lsp.DocumentURI]*lsp.TextDocumentItem
	inoDocsWithDiagnostics     map[lsp.DocumentURI]bool

	config       lsp.BoardConfig
	synchronizer Synchronizer
}

// NewInoHandler creates and configures an InoHandler.
func NewInoHandler(stdio io.ReadWriteCloser, board lsp.Board) *InoHandler {
	handler := &InoHandler{
		docs:                   map[lsp.DocumentURI]*lsp.TextDocumentItem{},
		inoDocsWithDiagnostics: map[lsp.DocumentURI]bool{},
		config: lsp.BoardConfig{
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

// StopClangd closes the connection to the clangd process.
func (handler *InoHandler) StopClangd() {
	handler.ClangdConn.Close()
	handler.ClangdConn = nil
}

// HandleMessageFromIDE handles a message received from the IDE client (via stdio).
func (handler *InoHandler) HandleMessageFromIDE(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error) {
	defer streams.CatchAndLogPanic()

	needsWriteLock := map[string]bool{
		"initialize":             true,
		"textDocument/didOpen":   true,
		"textDocument/didChange": true,
		"textDocument/didClose":  true,
	}
	if needsWriteLock[req.Method] {
		handler.synchronizer.DataMux.Lock()
		defer handler.synchronizer.DataMux.Unlock()
	} else {
		handler.synchronizer.DataMux.RLock()
		defer handler.synchronizer.DataMux.RUnlock()
	}

	// Handle LSP methods: transform parameters and send to clangd
	var inoURI, cppURI lsp.DocumentURI

	params, err := lsp.ReadParams(req.Method, req.Params)
	if err != nil {
		return nil, err
	}
	if params == nil {
		params = req.Params
	}
	switch p := params.(type) {
	case *lsp.InitializeParams:
		// method "initialize"
		err = handler.initializeWorkbench(p)

	case *lsp.InitializedParams:
		// method "initialized"
		log.Println("--> initialized")

	case *lsp.DidOpenTextDocumentParams:
		// method "textDocument/didOpen"
		inoURI = p.TextDocument.URI
		log.Printf("--> didOpen(%s@%d as '%s')", p.TextDocument.URI, p.TextDocument.Version, p.TextDocument.LanguageID)

		res, err := handler.didOpen(ctx, p)

		if res == nil {
			log.Println("    --X notification is not propagated to clangd")
			return nil, err // do not propagate to clangd
		}

		log.Printf("    --> didOpen(%s@%d as '%s')", res.TextDocument.URI, res.TextDocument.Version, p.TextDocument.LanguageID)
		params = res

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
		inoURI = p.TextDocument.URI
		log.Printf("--> completion(%s:%d:%d)\n", p.TextDocument.URI, p.Position.Line, p.Position.Character)

		if res, e := handler.ino2cppTextDocumentPositionParams(&p.TextDocumentPositionParams); e == nil {
			p.TextDocumentPositionParams = *res
			log.Printf("    --> completion(%s:%d:%d)\n", p.TextDocument.URI, p.Position.Line, p.Position.Character)
		} else {
			err = e
		}

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
		inoURI = p.TextDocument.URI
		doc := &p.TextDocumentPositionParams
		log.Printf("--> hover(%s:%d:%d)\n", doc.TextDocument.URI, doc.Position.Line, doc.Position.Character)

		if res, e := handler.ino2cppTextDocumentPositionParams(doc); e == nil {
			p.TextDocumentPositionParams = *res
			log.Printf("    --> hover(%s:%d:%d)\n", doc.TextDocument.URI, doc.Position.Line, doc.Position.Character)
		} else {
			err = e
		}

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

	case *lsp.TextDocumentPositionParams:
		// Method: "textDocument/signatureHelp"
		// Method: "textDocument/definition"
		// Method: "textDocument/typeDefinition"
		// Method: "textDocument/implementation"
		// Method: "textDocument/documentHighlight"
		log.Printf("--> %s(%s:%s)", req.Method, p.TextDocument.URI, p.Position)
		inoURI = p.TextDocument.URI
		if res, e := handler.ino2cppTextDocumentPositionParams(p); e == nil {
			params = res
			log.Printf("    --> %s(%s:%s)", req.Method, p.TextDocument.URI, p.Position)
		} else {
			err = e
		}

	case *lsp.DidSaveTextDocumentParams: // "textDocument/didSave":
		log.Printf("--X " + req.Method)
		return nil, nil
		inoURI = p.TextDocument.URI
		p.TextDocument, err = handler.ino2cppTextDocumentIdentifier(p.TextDocument)
	case *lsp.DidCloseTextDocumentParams: // "textDocument/didClose":
		log.Printf("--X " + req.Method)
		return nil, nil
		// uri = p.TextDocument.URI
		// err = handler.sketchToBuildPathTextDocumentIdentifier(&p.TextDocument)
		// handler.deleteFileData(uri)
	case *lsp.ReferenceParams: // "textDocument/references":
		log.Printf("--X " + req.Method)
		return nil, nil
		inoURI = p.TextDocument.URI
		_, err = handler.ino2cppTextDocumentPositionParams(&p.TextDocumentPositionParams)
	case *lsp.DocumentRangeFormattingParams: // "textDocument/rangeFormatting":
		log.Printf("--X " + req.Method)
		return nil, nil
		inoURI = p.TextDocument.URI
		err = handler.ino2cppDocumentRangeFormattingParams(p)
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
		log.Printf("    --E %s", err)
		return nil, err
	}

	var result interface{}
	if req.Notif {
		err = handler.ClangdConn.Notify(ctx, req.Method, params)
		// log.Println("    sent", req.Method, "notification to clangd")
	} else {
		ctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
		defer cancel()
		result, err = lsp.SendRequest(ctx, handler.ClangdConn, req.Method, params)
		// log.Println("    sent", req.Method, "request id", req.ID, " to clangd")
	}
	if err == nil && handler.buildSketchSymbolsLoad {
		handler.buildSketchSymbolsLoad = false
		log.Println("--! Resfreshing document symbols")
		err = handler.refreshCppDocumentSymbols()
	}
	if err == nil && handler.buildSketchSymbolsCheck {
		handler.buildSketchSymbolsCheck = false
		log.Println("--! Resfreshing document symbols")
		err = handler.checkCppDocumentSymbols()
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
		result = handler.transformClangdResult(req.Method, inoURI, cppURI, result)
	}
	return result, err
}

func (handler *InoHandler) exit() {
	log.Println("Please restart the language server.")
	handler.StopClangd()
	os.Exit(1)
}

func (handler *InoHandler) initializeWorkbench(params *lsp.InitializeParams) error {
	currCppTextVersion := 0
	if params != nil {
		log.Printf("--> initialize(%s)\n", params.RootURI)
		handler.lspInitializeParams = params
		handler.sketchRoot = params.RootURI.AsPath()
		handler.sketchName = handler.sketchRoot.Base()
	} else {
		currCppTextVersion = handler.sketchMapper.CppText.Version
		log.Printf("--> RE-initialize()\n")
	}

	if buildPath, err := handler.generateBuildEnvironment(); err == nil {
		handler.buildPath = buildPath
		handler.buildSketchRoot = buildPath.Join("sketch")
	} else {
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

	if params == nil {
		// If we are restarting re-synchronize clangd
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

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
	}

	return nil
}

func (handler *InoHandler) refreshCppDocumentSymbols() error {
	// Query source code symbols
	cppURI := lsp.NewDocumentURIFromPath(handler.buildSketchCpp)
	log.Printf("    --> documentSymbol(%s)", cppURI)
	result, err := lsp.SendRequest(context.Background(), handler.ClangdConn, "textDocument/documentSymbol", &lsp.DocumentSymbolParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: cppURI},
	})
	if err != nil {
		return errors.WithMessage(err, "quering source code symbols")
	}
	result = handler.transformClangdResult("textDocument/documentSymbol", cppURI, "", result)
	if symbols, ok := result.([]lsp.DocumentSymbol); !ok {
		return errors.WithMessage(err, "quering source code symbols (2)")
	} else {
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
		for _, symbol := range symbols {
			log.Printf("    symbol: %s %s", symbol.Kind, symbol.Name)
		}
		handler.buildSketchSymbols = symbols
	}
	return nil
}

func (handler *InoHandler) checkCppDocumentSymbols() error {
	oldSymbols := handler.buildSketchSymbols
	if err := handler.refreshCppDocumentSymbols(); err != nil {
		return err
	}
	if len(oldSymbols) != len(handler.buildSketchSymbols) {
		log.Println("--! new symbols detected, triggering sketch rebuild!")
		handler.scheduleRebuildEnvironment()
		return nil
	}
	for i, old := range oldSymbols {
		if newName := handler.buildSketchSymbols[i].Name; old.Name != newName {
			log.Printf("--! symbols changed, triggering sketch rebuild: '%s' -> '%s'", old.Name, newName)
			handler.scheduleRebuildEnvironment()
			return nil
		}
	}
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

func (handler *InoHandler) didOpen(ctx context.Context, inoDidOpen *lsp.DidOpenTextDocumentParams) (*lsp.DidOpenTextDocumentParams, error) {
	// Add the TextDocumentItem in the tracked files list
	inoItem := inoDidOpen.TextDocument
	handler.docs[inoItem.URI] = &inoItem

	// If we are tracking a .ino...
	if inoItem.URI.Ext() == ".ino" {
		handler.sketchTrackedFilesCount++
		log.Printf("    increasing .ino tracked files count: %d", handler.sketchTrackedFilesCount)

		// notify clang that sketchCpp has been opened only once
		if handler.sketchTrackedFilesCount != 1 {
			return nil, nil
		}

		// trigger a documentSymbol load
		handler.buildSketchSymbolsLoad = true
	}

	cppItem, err := handler.ino2cppTextDocumentItem(inoItem)
	return &lsp.DidOpenTextDocumentParams{
		TextDocument: cppItem,
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
		cppItem.Text = handler.docs[inoItem.URI].Text
		cppItem.Version = handler.docs[inoItem.URI].Version
	}

	return cppItem, nil
}

func (handler *InoHandler) didChange(ctx context.Context, req *lsp.DidChangeTextDocumentParams) (*lsp.DidChangeTextDocumentParams, error) {
	doc := req.TextDocument

	trackedDoc, ok := handler.docs[doc.URI]
	if !ok {
		return nil, unknownURI(doc.URI)
	}
	textutils.ApplyLSPTextDocumentContentChangeEvent(trackedDoc, req.ContentChanges, doc.Version)

	// If changes are applied to a .ino file we increment the global .ino.cpp versioning
	// for each increment of the single .ino file.
	if doc.URI.Ext() == ".ino" {

		cppChanges := []lsp.TextDocumentContentChangeEvent{}
		for _, inoChange := range req.ContentChanges {
			dirty := handler.sketchMapper.ApplyTextChange(doc.URI, inoChange)
			if dirty {
				// TODO: Detect changes in critical lines (for example function definitions)
				//       and trigger arduino-preprocessing + clangd restart.

				log.Println("--! DIRTY CHANGE, force sketch rebuild!")
				handler.scheduleRebuildEnvironment()
			}

			// log.Println("New version:----------")
			// log.Println(handler.sketchMapper.CppText.Text)
			// log.Println("----------------------")

			cppRange, ok := handler.sketchMapper.InoToCppLSPRangeOk(doc.URI, *inoChange.Range)
			if !ok {
				return nil, errors.Errorf("invalid change range %s:%s", doc.URI, *inoChange.Range)
			}
			cppChange := lsp.TextDocumentContentChangeEvent{
				Range:       &cppRange,
				RangeLength: inoChange.RangeLength,
				Text:        inoChange.Text,
			}
			cppChanges = append(cppChanges, cppChange)
		}

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
		return "", unknownURI(inoURI)
	}
	if !inside {
		log.Printf("    passing doc identifier to '%s' as-is", inoPath)
		return inoURI, nil
	}

	rel, err := handler.sketchRoot.RelTo(inoPath)
	if err == nil {
		cppPath := handler.buildSketchRoot.JoinPath(rel)
		log.Printf("    URI: '%s' -> '%s'", inoPath, cppPath)
		return lsp.NewDocumentURIFromPath(cppPath), nil
	}

	log.Printf("    could not determine rel-path of '%s' in '%s': %s", inoPath, handler.sketchRoot, err)
	return "", err
}

func (handler *InoHandler) cpp2inoDocumentURI(cppURI lsp.DocumentURI, cppRange lsp.Range) (lsp.DocumentURI, lsp.Range, error) {
	// Sketchbook/Sketch/Sketch.ino      <- build-path/sketch/Sketch.ino.cpp
	// Sketchbook/Sketch/AnotherTab.ino  <- build-path/sketch/Sketch.ino.cpp  (different section from above)
	// Sketchbook/Sketch/AnotherFile.cpp <- build-path/sketch/AnotherFile.cpp (1:1)
	// another/path/source.cpp           <- unchanged

	// Convert build path to sketch path
	cppPath := cppURI.AsPath()
	if cppPath.EquivalentTo(handler.buildSketchCpp) {
		inoPath, inoRange, err := handler.sketchMapper.CppToInoRangeOk(cppRange)
		if err == nil {
			log.Printf("    URI: converted %s to %s:%s", cppRange, inoPath, inoRange)
		} else if _, ok := err.(sourcemapper.AdjustedRangeErr); ok {
			log.Printf("    URI: converted %s to %s:%s (END LINE ADJUSTED)", cppRange, inoPath, inoRange)
			err = nil
		} else {
			log.Printf("    URI: ERROR: %s", err)
			handler.sketchMapper.DebugLogAll()
		}
		return lsp.NewDocumentURI(inoPath), inoRange, err
	}

	inside, err := cppPath.IsInsideDir(handler.buildSketchRoot)
	if err != nil {
		log.Printf("    could not determine if '%s' is inside '%s'", cppPath, handler.buildSketchRoot)
		return "", lsp.Range{}, err
	}
	if !inside {
		log.Printf("    keep doc identifier to '%s' as-is", cppPath)
		return cppURI, cppRange, nil
	}

	rel, err := handler.buildSketchRoot.RelTo(cppPath)
	if err == nil {
		inoPath := handler.sketchRoot.JoinPath(rel)
		log.Printf("    URI: '%s' -> '%s'", cppPath, inoPath)
		return lsp.NewDocumentURIFromPath(inoPath), cppRange, nil
	}

	log.Printf("    could not determine rel-path of '%s' in '%s': %s", cppPath, handler.buildSketchRoot, err)
	return "", lsp.Range{}, err
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

func (handler *InoHandler) ino2cppDocumentRangeFormattingParams(params *lsp.DocumentRangeFormattingParams) error {
	panic("not implemented")
	// handler.sketchToBuildPathTextDocumentIdentifier(&params.TextDocument)
	// if data, ok := handler.data[params.TextDocument.URI]; ok {
	// 	params.Range = data.sourceMap.InoToCppLSPRange(data.sourceURI, params.Range)
	// 	return nil
	// }
	return unknownURI(params.TextDocument.URI)
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
	cppToIno := inoURI != "" && inoURI.AsPath().EquivalentTo(handler.buildSketchCpp)

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
		log.Printf("<-- completion(%d items)", len(r.Items))
		return r

	case *lsp.DocumentSymbolArrayOrSymbolInformationArray:
		// method "textDocument/documentSymbol"

		if r.DocumentSymbolArray != nil {
			// Treat the input as []DocumentSymbol
			return handler.cpp2inoDocumentSymbols(*r.DocumentSymbolArray, inoURI)
		} else if r.SymbolInformationArray != nil {
			// Treat the input as []SymbolInformation
			return handler.cpp2inoSymbolInformation(*r.SymbolInformationArray)
		} else {
			// Treat the input as null
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

	case *[]lsp.DocumentHighlight: // "textDocument/documentHighlight":
		for index := range *r {
			handler.cpp2inoDocumentHighlight(&(*r)[index], inoURI)
		}
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

func (handler *InoHandler) cpp2inoWorkspaceEdit(origWorkspaceEdit *lsp.WorkspaceEdit) *lsp.WorkspaceEdit {
	if origWorkspaceEdit == nil {
		return nil
	}
	resWorkspaceEdit := &lsp.WorkspaceEdit{
		Changes: map[lsp.DocumentURI][]lsp.TextEdit{},
	}
	for editURI, edits := range origWorkspaceEdit.Changes {
		// if the edits are not relative to sketch file...
		if !editURI.AsPath().EquivalentTo(handler.buildSketchCpp) {
			// ...pass them through...
			resWorkspaceEdit.Changes[editURI] = edits
			continue
		}

		// ...otherwise convert edits to the sketch.ino.cpp into multilpe .ino edits
		for _, edit := range edits {
			cppRange := edit.Range
			inoFile, inoRange := handler.sketchMapper.CppToInoRange(cppRange)
			inoURI := lsp.NewDocumentURI(inoFile)
			if _, have := resWorkspaceEdit.Changes[inoURI]; !have {
				resWorkspaceEdit.Changes[inoURI] = []lsp.TextEdit{}
			}
			resWorkspaceEdit.Changes[inoURI] = append(resWorkspaceEdit.Changes[inoURI], lsp.TextEdit{
				NewText: edit.NewText,
				Range:   inoRange,
			})
		}
	}
	return resWorkspaceEdit
}

func (handler *InoHandler) cpp2inoLocation(inoLocation lsp.Location) (lsp.Location, error) {
	cppURI, cppRange, err := handler.cpp2inoDocumentURI(inoLocation.URI, inoLocation.Range)
	return lsp.Location{
		URI:   cppURI,
		Range: cppRange,
	}, err
}

func (handler *InoHandler) cpp2inoDocumentHighlight(highlight *lsp.DocumentHighlight, uri lsp.DocumentURI) {
	panic("not implemented")
	// if data, ok := handler.data[uri]; ok {
	// 	_, highlight.Range = data.sourceMap.CppToInoRange(highlight.Range)
	// }
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

func (handler *InoHandler) cpp2inoDocumentSymbols(origSymbols []lsp.DocumentSymbol, origURI lsp.DocumentURI) []lsp.DocumentSymbol {
	if origURI.Ext() != ".ino" || len(origSymbols) == 0 {
		return origSymbols
	}

	inoSymbols := []lsp.DocumentSymbol{}
	for _, symbol := range origSymbols {
		if handler.sketchMapper.IsPreprocessedCppLine(symbol.Range.Start.Line) {
			continue
		}

		inoFile, inoRange := handler.sketchMapper.CppToInoRange(symbol.Range)
		inoSelectionURI, inoSelectionRange := handler.sketchMapper.CppToInoRange(symbol.SelectionRange)

		if inoFile != inoSelectionURI {
			log.Printf("    ERROR: symbol range and selection belongs to different URI!")
			log.Printf("           > %s != %s", symbol.Range, symbol.SelectionRange)
			log.Printf("           > %s:%s != %s:%s", inoFile, inoRange, inoSelectionURI, inoSelectionRange)
			continue
		}

		if inoFile != origURI.Unbox() {
			//log.Printf("    skipping symbol related to %s", inoFile)
			continue
		}

		inoSymbols = append(inoSymbols, lsp.DocumentSymbol{
			Name:           symbol.Name,
			Detail:         symbol.Detail,
			Deprecated:     symbol.Deprecated,
			Kind:           symbol.Kind,
			Range:          inoRange,
			SelectionRange: inoSelectionRange,
			Children:       handler.cpp2inoDocumentSymbols(symbol.Children, origURI),
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

	if len(cppDiags.Diagnostics) == 0 {
		// If we receive the empty diagnostic on the preprocessed sketch,
		// just return an empty diagnostic array.
		if cppDiags.URI.AsPath().EquivalentTo(handler.buildSketchCpp) {
			return []*lsp.PublishDiagnosticsParams{}, nil
		}

		inoURI, _, err := handler.cpp2inoDocumentURI(cppDiags.URI, lsp.Range{})
		return []*lsp.PublishDiagnosticsParams{
			{
				URI:         inoURI,
				Diagnostics: []lsp.Diagnostic{},
			},
		}, err
	}

	convertedDiagnostics := map[lsp.DocumentURI]*lsp.PublishDiagnosticsParams{}
	for _, cppDiag := range cppDiags.Diagnostics {
		inoURI, inoRange, err := handler.cpp2inoDocumentURI(cppDiags.URI, cppDiag.Range)
		if err != nil {
			return nil, err
		}

		inoDiagParam, created := convertedDiagnostics[inoURI]
		if !created {
			inoDiagParam = &lsp.PublishDiagnosticsParams{
				URI:         inoURI,
				Diagnostics: []lsp.Diagnostic{},
			}
			convertedDiagnostics[inoURI] = inoDiagParam
		}

		inoDiag := cppDiag
		inoDiag.Range = inoRange
		inoDiagParam.Diagnostics = append(inoDiagParam.Diagnostics, inoDiag)
	}

	inoDiagParams := []*lsp.PublishDiagnosticsParams{}
	for _, v := range convertedDiagnostics {
		inoDiagParams = append(inoDiagParams, v)
	}
	return inoDiagParams, nil
}

// FromClangd handles a message received from clangd.
func (handler *InoHandler) FromClangd(ctx context.Context, connection *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error) {
	defer streams.CatchAndLogPanic()

	handler.synchronizer.DataMux.RLock()
	defer handler.synchronizer.DataMux.RUnlock()

	params, err := lsp.ReadParams(req.Method, req.Params)
	if err != nil {
		return nil, errors.WithMessage(err, "parsing JSON message from clangd")
	}
	if params == nil {
		// passthrough
		params = req.Params
	}
	switch p := params.(type) {
	case *lsp.PublishDiagnosticsParams:
		// "textDocument/publishDiagnostics"
		log.Printf("    <-- publishDiagnostics(%s):", p.URI)
		for _, diag := range p.Diagnostics {
			log.Printf("        > %d:%d - %v: %s", diag.Range.Start.Line, diag.Range.Start.Character, diag.Severity, diag.Code)
		}

		// the diagnostics on sketch.cpp.ino once mapped into their
		// .ino counter parts may span over multiple .ino files...
		inoDiagnostics, err := handler.cpp2inoDiagnostics(p)
		if err != nil {
			return nil, err
		}
		cleanUpInoDiagnostics := false
		if len(inoDiagnostics) == 0 {
			cleanUpInoDiagnostics = true
		}

		// Push back to IDE the converted diagnostics
		inoDocsWithDiagnostics := map[lsp.DocumentURI]bool{}
		for _, inoDiag := range inoDiagnostics {
			if enableLogging {
				log.Printf("<-- publishDiagnostics(%s):", inoDiag.URI)
				for _, diag := range inoDiag.Diagnostics {
					log.Printf("    > %d:%d - %v: %s", diag.Range.Start.Line, diag.Range.Start.Character, diag.Severity, diag.Code)
				}
			}

			// If we have an "undefined reference" in the .ino code trigger a
			// check for newly created symbols (that in turn may trigger a
			// new arduino-preprocessing of the sketch).
			if inoDiag.URI.Ext() == ".ino" {
				inoDocsWithDiagnostics[inoDiag.URI] = true
				cleanUpInoDiagnostics = true
				for _, diag := range inoDiag.Diagnostics {
					if diag.Code == "undeclared_var_use_suggest" || diag.Code == "undeclared_var_use" {
						handler.buildSketchSymbolsCheck = true
					}
				}
			}

			if err := handler.StdioConn.Notify(ctx, "textDocument/publishDiagnostics", inoDiag); err != nil {
				return nil, err
			}
		}

		if cleanUpInoDiagnostics {
			// Remove diagnostics from all .ino where there are no errors coming from clang
			for sourceURI := range handler.inoDocsWithDiagnostics {
				if inoDocsWithDiagnostics[sourceURI] {
					// skip if we already sent updated diagnostics
					continue
				}
				// otherwise clear previous diagnostics
				msg := lsp.PublishDiagnosticsParams{
					URI:         sourceURI,
					Diagnostics: []lsp.Diagnostic{},
				}
				if enableLogging {
					log.Printf("<-- publishDiagnostics(%s):", msg.URI)
				}
				if err := handler.StdioConn.Notify(ctx, "textDocument/publishDiagnostics", msg); err != nil {
					return nil, err
				}
			}

			handler.inoDocsWithDiagnostics = inoDocsWithDiagnostics
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
	var result interface{}
	if req.Notif {
		err = handler.StdioConn.Notify(ctx, req.Method, params)
		if enableLogging {
			log.Println("From clangd:", req.Method)
		}
	} else {
		result, err = lsp.SendRequest(ctx, handler.StdioConn, req.Method, params)
		if enableLogging {
			log.Println("From clangd:", req.Method, "id", req.ID)
		}
	}
	return result, err
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
	return errors.New("Document is not available: " + string(uri))
}
