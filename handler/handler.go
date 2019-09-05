package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/url"
	"path/filepath"
	"strings"

	lsp "github.com/sourcegraph/go-lsp"
	"github.com/sourcegraph/jsonrpc2"
)

var globalCliPath string
var enableLogging bool

// Setup initializes global variables.
func Setup(cliPath string, _enableLogging bool) {
	globalCliPath = cliPath
	enableLogging = _enableLogging
}

// NewInoHandler creates and configures an InoHandler.
func NewInoHandler(stdin io.ReadCloser, stdout io.WriteCloser, stdinLog, stdoutLog io.Writer,
	clangdIn io.ReadCloser, clangdOut io.WriteCloser, clangdinLog, clangdoutLog io.Writer) *InoHandler {
	handler := &InoHandler{
		data: make(map[lsp.DocumentURI]*FileData),
	}
	clangdStream := jsonrpc2.NewBufferedStream(StreamReadWrite{clangdIn, clangdOut, clangdinLog, clangdoutLog}, jsonrpc2.VSCodeObjectCodec{})
	clangdHandler := jsonrpc2.HandlerWithError(handler.FromClangd)
	handler.ClangdConn = jsonrpc2.NewConn(context.Background(), clangdStream, clangdHandler)
	stdStream := jsonrpc2.NewBufferedStream(StreamReadWrite{stdin, stdout, stdinLog, stdoutLog}, jsonrpc2.VSCodeObjectCodec{})
	stdHandler := jsonrpc2.HandlerWithError(handler.FromStdio)
	handler.StdioConn = jsonrpc2.NewConn(context.Background(), stdStream, stdHandler)
	return handler
}

// InoHandler is a JSON-RPC handler that delegates messages to clangd.
type InoHandler struct {
	StdioConn  *jsonrpc2.Conn
	ClangdConn *jsonrpc2.Conn
	data       map[lsp.DocumentURI]*FileData
}

// FileData gathers information on a .ino source file.
type FileData struct {
	sourceText    string
	sourceURI     lsp.DocumentURI
	targetURI     lsp.DocumentURI
	sourceLineMap map[int]int
	targetLineMap map[int]int
}

// FromStdio handles a message received from the client (via stdio).
func (handler *InoHandler) FromStdio(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error) {
	params, uri, err := handler.transformClangdParams(req.Method, req.Params)
	if err != nil {
		log.Println("From stdio: Method:", req.Method, "Error:", err)
		return nil, err
	}
	var result interface{}
	if req.Notif {
		err = handler.ClangdConn.Notify(ctx, req.Method, params)
	} else {
		result, err = sendRequest(ctx, handler.ClangdConn, req.Method, params)
	}
	if err != nil {
		log.Println("From stdio: Method:", req.Method, "Error:", err)
		return nil, err
	}
	if enableLogging {
		log.Println("From stdio:", req.Method)
	}
	if result != nil {
		return handler.transformClangdResult(req.Method, uri, result), nil
	}
	return result, err
}

func (handler *InoHandler) transformClangdParams(method string, raw *json.RawMessage) (params interface{}, uri lsp.DocumentURI, err error) {
	params, err = readParams(method, raw)
	if err != nil {
		return
	} else if params == nil {
		params = raw
		return
	}
	switch method {
	case "textDocument/didOpen":
		p := params.(*lsp.DidOpenTextDocumentParams)
		uri = p.TextDocument.URI
		err = handler.ino2cppTextDocumentItem(&p.TextDocument)
	case "textDocument/didChange":
		p := params.(*lsp.DidChangeTextDocumentParams)
		uri = p.TextDocument.URI
		err = handler.ino2cppDidChangeTextDocumentParams(p)
	case "textDocument/didSave":
		p := params.(*lsp.DidSaveTextDocumentParams)
		uri = p.TextDocument.URI
		err = handler.ino2cppTextDocumentIdentifier(&p.TextDocument)
	case "textDocument/didClose":
		p := params.(*lsp.DidCloseTextDocumentParams)
		uri = p.TextDocument.URI
		handler.deleteFileData(uri)
		err = handler.ino2cppTextDocumentIdentifier(&p.TextDocument)
	case "textDocument/completion":
		p := params.(*lsp.CompletionParams)
		uri = p.TextDocument.URI
		err = handler.ino2cppTextDocumentPositionParams(&p.TextDocumentPositionParams)
	case "textDocument/codeAction":
		p := params.(*lsp.CodeActionParams)
		uri = p.TextDocument.URI
		err = handler.ino2cppCodeActionParams(p)
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
		// TODO
	case "textDocument/onTypeFormatting":
		// TODO
	}
	return
}

func (handler *InoHandler) createFileData(sourceURI lsp.DocumentURI, sourceText string) (*FileData, []byte, error) {
	sourcePath := uriToPath(sourceURI)
	// TODO get board from sketch config
	fqbn := "arduino:avr:uno"
	targetPath, targetBytes, err := generateCpp([]byte(sourceText), filepath.Base(sourcePath), fqbn)
	if err != nil {
		return nil, nil, err
	}

	targetURI := pathToURI(targetPath)
	sourceLineMap, targetLineMap := createSourceMaps(bytes.NewReader(targetBytes))
	data := &FileData{
		sourceText,
		sourceURI,
		targetURI,
		sourceLineMap,
		targetLineMap,
	}
	handler.data[sourceURI] = data
	handler.data[targetURI] = data
	return data, targetBytes, nil
}

func (handler *InoHandler) updateFileData(data *FileData, change *lsp.TextDocumentContentChangeEvent) error {
	rang := change.Range
	if rang == nil || rang.Start.Line != rang.End.Line {
		// Update the source text and regenerate the cpp code
		var newSourceText string
		if rang == nil {
			newSourceText = change.Text
		} else {
			newSourceText = applyTextChange(data.sourceText, *rang, change.Text)
		}
		// TODO get board from sketch config
		fqbn := "arduino:avr:uno"
		targetBytes, err := updateCpp([]byte(newSourceText), fqbn, uriToPath(data.targetURI))
		if err != nil {
			return err
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
		data.sourceText = applyTextChange(data.sourceText, *rang, change.Text)
		updateSourceMaps(data.sourceLineMap, data.targetLineMap, rang.Start.Line, change.Text)

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

func (handler *InoHandler) ino2cppTextDocumentItem(doc *lsp.TextDocumentItem) error {
	if strings.HasSuffix(string(doc.URI), ".ino") {
		data, targetBytes, err := handler.createFileData(doc.URI, doc.Text)
		if err != nil {
			return err
		}
		doc.LanguageID = "cpp"
		doc.URI = data.targetURI
		doc.Text = string(targetBytes)
	}
	return nil
}

func (handler *InoHandler) ino2cppDidChangeTextDocumentParams(params *lsp.DidChangeTextDocumentParams) error {
	handler.ino2cppTextDocumentIdentifier(&params.TextDocument.TextDocumentIdentifier)
	if data, ok := handler.data[params.TextDocument.URI]; ok {
		for index := range params.ContentChanges {
			err := handler.updateFileData(data, &params.ContentChanges[index])
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (handler *InoHandler) ino2cppTextDocumentPositionParams(params *lsp.TextDocumentPositionParams) error {
	handler.ino2cppTextDocumentIdentifier(&params.TextDocument)
	if data, ok := handler.data[params.TextDocument.URI]; ok {
		targetLine := data.targetLineMap[params.Position.Line]
		params.Position.Line = targetLine
	}
	return nil
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
	}
	return nil
}

func (handler *InoHandler) ino2cppTextDocumentIdentifier(doc *lsp.TextDocumentIdentifier) error {
	if data, ok := handler.data[doc.URI]; ok {
		doc.URI = data.targetURI
	}
	return nil
}

func (handler *InoHandler) transformClangdResult(method string, uri lsp.DocumentURI, result interface{}) interface{} {
	switch method {
	case "textDocument/completion":
		r := result.(*lsp.CompletionList)
		handler.cpp2inoCompletionList(r, uri)
	case "textDocument/codeAction":
		r := result.(*[]CodeAction)
		for index := range *r {
			handler.cpp2inoCodeAction(&(*r)[index], uri)
		}
	case "textDocument/hover":
		r := result.(*Hover)
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
	}
	return result
}

func (handler *InoHandler) cpp2inoCompletionList(list *lsp.CompletionList, uri lsp.DocumentURI) {
	if data, ok := handler.data[uri]; ok {
		for _, item := range list.Items {
			if item.TextEdit != nil {
				r := &item.TextEdit.Range
				r.Start.Line = data.sourceLineMap[r.Start.Line]
				r.End.Line = data.sourceLineMap[r.End.Line]
			}
		}
	}
}

func (handler *InoHandler) cpp2inoCodeAction(codeAction *CodeAction, uri lsp.DocumentURI) {
	newEdit := lsp.WorkspaceEdit{Changes: make(map[string][]lsp.TextEdit)}
	for uri, edit := range codeAction.Edit.Changes {
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
	codeAction.Edit = &newEdit
	if data, ok := handler.data[uri]; ok {
		for index := range codeAction.Diagnostics {
			r := &codeAction.Diagnostics[index].Range
			r.Start.Line = data.sourceLineMap[r.Start.Line]
			r.End.Line = data.sourceLineMap[r.End.Line]
		}
	}
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

// FromClangd handles a message received from clangd.
func (handler *InoHandler) FromClangd(ctx context.Context, connection *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error) {
	params, _, err := handler.transformStdioParams(req.Method, req.Params)
	if err != nil {
		log.Println("From clangd: Method:", req.Method, "Error:", err)
		return nil, err
	}
	var result interface{}
	if req.Notif {
		err = handler.StdioConn.Notify(ctx, req.Method, params)
	} else {
		result, err = sendRequest(ctx, handler.StdioConn, req.Method, params)
	}
	if err != nil {
		log.Println("From clangd: Method:", req.Method, "Error:", err)
		return nil, err
	}
	if enableLogging {
		log.Println("From clangd:", req.Method)
	}
	return result, err
}

func (handler *InoHandler) transformStdioParams(method string, raw *json.RawMessage) (params interface{}, uri lsp.DocumentURI, err error) {
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
	}
	return
}

func (handler *InoHandler) cpp2inoPublishDiagnosticsParams(params *lsp.PublishDiagnosticsParams) error {
	if data, ok := handler.data[params.URI]; ok {
		params.URI = data.sourceURI
		for index := range params.Diagnostics {
			r := &params.Diagnostics[index].Range
			r.Start.Line = data.sourceLineMap[r.Start.Line]
			r.End.Line = data.sourceLineMap[r.End.Line]
		}
	}
	return nil
}

func uriToPath(uri lsp.DocumentURI) string {
	url, err := url.Parse(string(uri))
	if err != nil {
		return string(uri)
	}
	return filepath.FromSlash(url.Path)
}

func pathToURI(path string) lsp.DocumentURI {
	return "file://" + lsp.DocumentURI(filepath.ToSlash(path))
}
