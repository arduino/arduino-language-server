package handler

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
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

// NewInoHandler creates and configures an InoHandler.
func NewInoHandler(stdio io.ReadWriteCloser, board lsp.Board) *InoHandler {
	handler := &InoHandler{
		//data:         map[lsp.DocumentURI]*FileData{},
		trackedFiles: map[lsp.DocumentURI]*lsp.TextDocumentItem{},
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
	return handler
}

// InoHandler is a JSON-RPC handler that delegates messages to clangd.
type InoHandler struct {
	StdioConn               *jsonrpc2.Conn
	ClangdConn              *jsonrpc2.Conn
	buildPath               *paths.Path
	buildSketchRoot         *paths.Path
	buildSketchCpp          *paths.Path
	buildSketchCppVersion   int
	sketchRoot              *paths.Path
	sketchName              string
	sketchMapper            *sourcemapper.InoMapper
	sketchTrackedFilesCount int
	trackedFiles            map[lsp.DocumentURI]*lsp.TextDocumentItem

	//data         map[lsp.DocumentURI]*FileData
	config       lsp.BoardConfig
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
		handler.synchronizer.DataMux.Lock()
		defer handler.synchronizer.DataMux.Unlock()
	} else {
		handler.synchronizer.DataMux.RLock()
		defer handler.synchronizer.DataMux.RUnlock()
	}

	// Handle LSP methods: transform parameters and send to clangd
	var uri lsp.DocumentURI

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
		err = handler.initializeWorkbench(ctx, p)

	case *lsp.DidOpenTextDocumentParams:
		// method "textDocument/didOpen"
		uri = p.TextDocument.URI
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
		uri = p.TextDocument.URI
		log.Printf("--> didChange(%s@%d)", p.TextDocument.URI, p.TextDocument.Version)
		for _, change := range p.ContentChanges {
			log.Printf("     > %s -> '%s'", change.Range, strconv.Quote(change.Text))
		}

		res, err := handler.didChange(ctx, p)
		if err != nil {
			log.Printf("    --E error: %s", err)
			return nil, err
		}
		if res == nil {
			log.Println("    --X notification is not propagated to clangd")
			return nil, err // do not propagate to clangd
		}

		p = res
		log.Printf("    --> didChange(%s@%d)", p.TextDocument.URI, p.TextDocument.Version)
		for _, change := range p.ContentChanges {
			log.Printf("         > %s -> '%s'", change.Range, strconv.Quote(change.Text))
		}
		err = handler.ClangdConn.Notify(ctx, req.Method, p)
		return nil, err

	case *lsp.CompletionParams:
		// method: "textDocument/completion"
		uri = p.TextDocument.URI
		log.Printf("--> completion(%s:%d:%d)\n", p.TextDocument.URI, p.Position.Line, p.Position.Character)

		err = handler.ino2cppTextDocumentPositionParams(&p.TextDocumentPositionParams)
		log.Printf("    --> completion(%s:%d:%d)\n", p.TextDocument.URI, p.Position.Line, p.Position.Character)

	case *lsp.CodeActionParams:
		// method "textDocument/codeAction"
		uri = p.TextDocument.URI
		log.Printf("--> codeAction(%s:%s)", p.TextDocument.URI, p.Range.Start)

		if err := handler.sketchToBuildPathTextDocumentIdentifier(&p.TextDocument); err != nil {
			break
		}
		if p.TextDocument.URI.AsPath().EquivalentTo(handler.buildSketchCpp) {
			p.Range = handler.sketchMapper.InoToCppLSPRange(uri, p.Range)
			for index := range p.Context.Diagnostics {
				r := &p.Context.Diagnostics[index].Range
				*r = handler.sketchMapper.InoToCppLSPRange(uri, *r)
			}
		}
		log.Printf("    --> codeAction(%s:%s)", p.TextDocument.URI, p.Range.Start)

	case *lsp.HoverParams:
		// method: "textDocument/hover"
		uri = p.TextDocument.URI
		doc := &p.TextDocumentPositionParams
		log.Printf("--> hover(%s:%d:%d)\n", doc.TextDocument.URI, doc.Position.Line, doc.Position.Character)

		err = handler.ino2cppTextDocumentPositionParams(doc)
		log.Printf("    --> hover(%s:%d:%d)\n", doc.TextDocument.URI, doc.Position.Line, doc.Position.Character)

	case *lsp.DocumentSymbolParams:
		// method "textDocument/documentSymbol"
		uri = p.TextDocument.URI
		log.Printf("--> documentSymbol(%s)", p.TextDocument.URI)

		err = handler.sketchToBuildPathTextDocumentIdentifier(&p.TextDocument)
		log.Printf("    --> documentSymbol(%s)", p.TextDocument.URI)

	case *lsp.DidSaveTextDocumentParams: // "textDocument/didSave":
		log.Printf("--X " + req.Method)
		return nil, nil
		uri = p.TextDocument.URI
		err = handler.sketchToBuildPathTextDocumentIdentifier(&p.TextDocument)
	case *lsp.DidCloseTextDocumentParams: // "textDocument/didClose":
		log.Printf("--X " + req.Method)
		return nil, nil
		uri = p.TextDocument.URI
		err = handler.sketchToBuildPathTextDocumentIdentifier(&p.TextDocument)
		handler.deleteFileData(uri)
	// case "textDocument/signatureHelp":
	// 	fallthrough
	// case "textDocument/definition":
	// 	fallthrough
	// case "textDocument/typeDefinition":
	// 	fallthrough
	// case "textDocument/implementation":
	// 	fallthrough
	case *lsp.TextDocumentPositionParams: // "textDocument/documentHighlight":
		log.Printf("--X " + req.Method)
		return nil, nil
		uri = p.TextDocument.URI
		err = handler.ino2cppTextDocumentPositionParams(p)
	case *lsp.ReferenceParams: // "textDocument/references":
		log.Printf("--X " + req.Method)
		return nil, nil
		uri = p.TextDocument.URI
		err = handler.ino2cppTextDocumentPositionParams(&p.TextDocumentPositionParams)
	case *lsp.DocumentFormattingParams: // "textDocument/formatting":
		log.Printf("--X " + req.Method)
		return nil, nil
		uri = p.TextDocument.URI
		err = handler.sketchToBuildPathTextDocumentIdentifier(&p.TextDocument)
	case *lsp.DocumentRangeFormattingParams: // "textDocument/rangeFormatting":
		log.Printf("--X " + req.Method)
		return nil, nil
		uri = p.TextDocument.URI
		err = handler.ino2cppDocumentRangeFormattingParams(p)
	case *lsp.DocumentOnTypeFormattingParams: // "textDocument/onTypeFormatting":
		log.Printf("--X " + req.Method)
		return nil, nil
		uri = p.TextDocument.URI
		err = handler.ino2cppDocumentOnTypeFormattingParams(p)
	case *lsp.RenameParams: // "textDocument/rename":
		log.Printf("--X " + req.Method)
		return nil, nil
		uri = p.TextDocument.URI
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
		if enableLogging {
			log.Println("    sent", req.Method, "notification to clangd")
		}
	} else {
		ctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
		defer cancel()
		result, err = lsp.SendRequest(ctx, handler.ClangdConn, req.Method, params)
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

func (handler *InoHandler) initializeWorkbench(ctx context.Context, params *lsp.InitializeParams) error {
	rootURI := params.RootURI
	log.Printf("--> initializeWorkbench(%s)\n", rootURI)

	handler.sketchRoot = rootURI.AsPath()
	handler.sketchName = handler.sketchRoot.Base()
	if buildPath, err := generateBuildEnvironment(handler.sketchRoot, handler.config.SelectedBoard.Fqbn); err == nil {
		handler.buildPath = buildPath
		handler.buildSketchRoot = buildPath.Join("sketch")
	} else {
		return err
	}
	handler.buildSketchCpp = handler.buildSketchRoot.Join(handler.sketchName + ".ino.cpp")
	handler.buildSketchCppVersion = 1

	if cppContent, err := handler.buildSketchCpp.ReadFile(); err == nil {
		handler.sketchMapper = sourcemapper.CreateInoMapper(cppContent)
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
	params.RootURI = lsp.NewDocumenteURIFromPath(handler.buildSketchRoot)
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
	handler.trackedFiles[doc.URI] = &doc

	// If we are tracking a .ino...
	if doc.URI.AsPath().Ext() == ".ino" {
		handler.sketchTrackedFilesCount++
		log.Printf("    increasing .ino tracked files count: %d", handler.sketchTrackedFilesCount)

		// ...notify clang that sketchCpp is no longer valid on disk
		if handler.sketchTrackedFilesCount == 1 {
			sketchCpp, err := handler.buildSketchCpp.ReadFile()
			newParam := &lsp.DidOpenTextDocumentParams{
				TextDocument: lsp.TextDocumentItem{
					URI:        lsp.NewDocumenteURIFromPath(handler.buildSketchCpp),
					Text:       string(sketchCpp),
					LanguageID: "cpp",
					Version:    handler.buildSketchCppVersion,
				},
			}
			return newParam, err
		}
	}
	return nil, nil
}

func (handler *InoHandler) didChange(ctx context.Context, req *lsp.DidChangeTextDocumentParams) (*lsp.DidChangeTextDocumentParams, error) {
	doc := req.TextDocument

	trackedDoc, ok := handler.trackedFiles[doc.URI]
	if !ok {
		return nil, unknownURI(doc.URI)
	}
	if trackedDoc.Version+1 != doc.Version {
		return nil, errors.Errorf("document out-of-sync: expected version %d but got %d", trackedDoc.Version+1, doc.Version)
	}
	trackedDoc.Version++

	if doc.URI.AsPath().Ext() == ".ino" {
		// If changes are applied to a .ino file we increment the global .ino.cpp versioning
		// for each increment of the single .ino file.

		cppChanges := []lsp.TextDocumentContentChangeEvent{}
		for _, inoChange := range req.ContentChanges {
			dirty := handler.sketchMapper.ApplyTextChange(doc.URI, inoChange)
			if dirty {
				// TODO: Detect changes in critical lines (for example function definitions)
				//       and trigger arduino-preprocessing + clangd restart.

				log.Println("    uh oh DIRTY CHANGE!")
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
					URI: lsp.NewDocumenteURIFromPath(handler.buildSketchCpp),
				},
				Version: handler.sketchMapper.CppText.Version,
			},
		}
		return cppReq, nil
	} else {

		// TODO
		return nil, unknownURI(doc.URI)

	}

	return nil, unknownURI(doc.URI)
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
		targetBytes, err := updateCpp([]byte(newSourceText), data.sourceURI.Unbox(), handler.config.SelectedBoard.Fqbn, false, data.targetURI.Unbox())
		if err != nil {
			if rang == nil {
				// Fallback: use the source text unchanged
				targetBytes, err = copyIno2Cpp(newSourceText, data.targetURI.Unbox())
				if err != nil {
					return err
				}
			} else {
				// Fallback: try to apply a multi-line update
				data.sourceText = newSourceText
				//data.sourceMap.Update(rang.End.Line-rang.Start.Line, rang.Start.Line, change.Text)
				*rang = data.sourceMap.InoToCppLSPRange(data.sourceURI, *rang)
				return nil
			}
		}

		data.sourceText = newSourceText
		data.sourceMap = sourcemapper.CreateInoMapper(targetBytes)

		change.Text = string(targetBytes)
		change.Range = nil
		change.RangeLength = 0
	} else {
		// Apply an update to a single line both to the source and the target text
		data.sourceText, err = textutils.ApplyTextChange(data.sourceText, *rang, change.Text)
		if err != nil {
			return err
		}
		//data.sourceMap.Update(0, rang.Start.Line, change.Text)

		*rang = data.sourceMap.InoToCppLSPRange(data.sourceURI, *rang)
	}
	return nil
}

func (handler *InoHandler) deleteFileData(sourceURI lsp.DocumentURI) {
	// if data, ok := handler.data[sourceURI]; ok {
	// 	delete(handler.data, data.sourceURI)
	// 	delete(handler.data, data.targetURI)
	// }
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
	docFile := doc.URI.AsPath()
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
	doc.URI = lsp.NewDocumenteURIFromPath(newDocFile)
	return nil
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

func (handler *InoHandler) transformClangdResult(method string, uri lsp.DocumentURI, result interface{}) interface{} {
	handler.synchronizer.DataMux.RLock()
	defer handler.synchronizer.DataMux.RUnlock()

	cppToIno := uri != "" && uri.AsPath().EquivalentTo(handler.buildSketchCpp)

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
			return handler.cpp2inoDocumentSymbols(*r.DocumentSymbolArray, uri)
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
			(*r)[i] = lsp.CommandOrCodeAction{
				Command:    handler.cpp2inoCommand(item.Command),
				CodeAction: handler.cpp2inoCodeAction(item.CodeAction, uri),
			}
			if item.Command != nil {
				log.Printf("        > Command: %s", item.Command.Title)
			}
			if item.CodeAction != nil {
				log.Printf("        > CodeAction: %s", item.CodeAction.Title)
			}
		}
		log.Printf("<-- codeAction(%d elements)", len(*r))

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
	case *lsp.WorkspaceEdit: // "textDocument/rename":
		return handler.cpp2inoWorkspaceEdit(r)
	case *[]lsp.SymbolInformation: // "workspace/symbol":
		for index := range *r {
			handler.cpp2inoLocation(&(*r)[index].Location)
		}
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
		Command:     handler.cpp2inoCommand(codeAction.Command),
	}
	if uri.AsPath().Ext() == ".ino" {
		for i, diag := range inoCodeAction.Diagnostics {
			_, inoCodeAction.Diagnostics[i].Range = handler.sketchMapper.CppToInoRange(diag.Range)
		}
	}
	return inoCodeAction
}

func (handler *InoHandler) cpp2inoCommand(command *lsp.Command) *lsp.Command {
	if command == nil {
		return nil
	}
	inoCommand := &lsp.Command{
		Title:     command.Title,
		Command:   command.Command,
		Arguments: command.Arguments,
	}
	if len(command.Arguments) == 1 {
		arg := handler.parseCommandArgument(inoCommand.Arguments[0])
		if workspaceEdit, ok := arg.(*lsp.WorkspaceEdit); ok {
			inoCommand.Arguments[0] = handler.cpp2inoWorkspaceEdit(workspaceEdit)
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

func (handler *InoHandler) cpp2inoLocation(location *lsp.Location) {
	panic("not implemented")
	// if data, ok := handler.data[location.URI]; ok {
	// 	location.URI = data.sourceURI
	// 	_, location.Range = data.sourceMap.CppToInoRange(location.Range)
	// }
}

func (handler *InoHandler) cpp2inoDocumentHighlight(highlight *lsp.DocumentHighlight, uri lsp.DocumentURI) {
	panic("not implemented")
	// if data, ok := handler.data[uri]; ok {
	// 	_, highlight.Range = data.sourceMap.CppToInoRange(highlight.Range)
	// }
}

func (handler *InoHandler) cpp2inoTextEdit(edit *lsp.TextEdit, uri lsp.DocumentURI) {
	panic("not implemented")
	// if data, ok := handler.data[uri]; ok {
	// 	_, edit.Range = data.sourceMap.CppToInoRange(edit.Range)
	// }
}

func (handler *InoHandler) cpp2inoDocumentSymbols(origSymbols []lsp.DocumentSymbol, origURI lsp.DocumentURI) []lsp.DocumentSymbol {
	if origURI.AsPath().Ext() != ".ino" || len(origSymbols) == 0 {
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

// FromClangd handles a message received from clangd.
func (handler *InoHandler) FromClangd(ctx context.Context, connection *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error) {
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

		if p.URI.AsPath().EquivalentTo(handler.buildSketchCpp) {
			// we should transform back N diagnostics of sketch.cpp.ino into
			// their .ino counter parts (that may span over multiple files...)

			// Remove diagnostics from all .ino if there are no errors coming from clang
			if len(p.Diagnostics) == 0 {
				// XXX: Optimize this to publish "empty diagnostics" only to .ino that are
				//      currently showing previous diagnostics.

				for sourceURI := range handler.trackedFiles {
					msg := lsp.PublishDiagnosticsParams{
						URI:         sourceURI,
						Diagnostics: []lsp.Diagnostic{},
					}
					if err := handler.StdioConn.Notify(ctx, "textDocument/publishDiagnostics", msg); err != nil {
						return nil, err
					}
				}
				return nil, nil
			}

			convertedDiagnostics := map[string][]lsp.Diagnostic{}
			for _, cppDiag := range p.Diagnostics {
				inoSource, inoRange := handler.sketchMapper.CppToInoRange(cppDiag.Range)
				inoDiag := cppDiag
				inoDiag.Range = inoRange
				if inoDiags, ok := convertedDiagnostics[inoSource]; !ok {
					convertedDiagnostics[inoSource] = []lsp.Diagnostic{inoDiag}
				} else {
					convertedDiagnostics[inoSource] = append(inoDiags, inoDiag)
				}
			}

			// Push back to IDE the converted diagnostics
			for filename, inoDiags := range convertedDiagnostics {
				msg := lsp.PublishDiagnosticsParams{
					URI:         lsp.NewDocumentURI(filename),
					Diagnostics: inoDiags,
				}
				if enableLogging {
					log.Printf("<-- publishDiagnostics(%s):", msg.URI)
					for _, diag := range msg.Diagnostics {
						log.Printf("    > %d:%d - %v: %s", diag.Range.Start.Line, diag.Range.Start.Character, diag.Severity, diag.Code)
					}
				}
				if err := handler.StdioConn.Notify(ctx, "textDocument/publishDiagnostics", msg); err != nil {
					return nil, err
				}
			}

			return nil, err
		}

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

func (handler *InoHandler) parseCommandArgument(rawArg interface{}) interface{} {
	log.Printf("        TRY TO PARSE: %+v", rawArg)
	panic("not implemented")
	return nil
	// if m1, ok := rawArg.(map[string]interface{}); ok && len(m1) == 1 && m1["changes"] != nil {
	// 	m2 := m1["changes"].(map[string]interface{})
	// 	workspaceEdit := lsp.WorkspaceEdit{Changes: make(map[string][]lsp.TextEdit)}
	// 	for uri, rawValue := range m2 {
	// 		rawTextEdits := rawValue.([]interface{})
	// 		textEdits := make([]lsp.TextEdit, len(rawTextEdits))
	// 		for index := range rawTextEdits {
	// 			m3 := rawTextEdits[index].(map[string]interface{})
	// 			rawRange := m3["range"]
	// 			m4 := rawRange.(map[string]interface{})
	// 			rawStart := m4["start"]
	// 			m5 := rawStart.(map[string]interface{})
	// 			textEdits[index].Range.Start.Line = int(m5["line"].(float64))
	// 			textEdits[index].Range.Start.Character = int(m5["character"].(float64))
	// 			rawEnd := m4["end"]
	// 			m6 := rawEnd.(map[string]interface{})
	// 			textEdits[index].Range.End.Line = int(m6["line"].(float64))
	// 			textEdits[index].Range.End.Character = int(m6["character"].(float64))
	// 			textEdits[index].NewText = m3["newText"].(string)
	// 		}
	// 		workspaceEdit.Changes[uri] = textEdits
	// 	}
	// 	return &workspaceEdit
	// }
	// return nil
}

func (handler *InoHandler) showMessage(ctx context.Context, msgType lsp.MessageType, message string) {
	params := lsp.ShowMessageParams{
		Type:    msgType,
		Message: message,
	}
	handler.StdioConn.Notify(ctx, "window/showMessage", &params)
}

func unknownURI(uri lsp.DocumentURI) error {
	return errors.New("Document is not available: " + string(uri))
}
