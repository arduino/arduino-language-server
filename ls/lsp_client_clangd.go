package ls

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/arduino/arduino-cli/executils"
	"github.com/arduino/arduino-language-server/streams"
	"github.com/arduino/go-paths-helper"
	"go.bug.st/json"
	"go.bug.st/lsp"
	"go.bug.st/lsp/jsonrpc"
)

type ClangdLSPClient struct {
	conn *lsp.Client
	ls   *INOLanguageServer
}

func NewClangdLSPClient(logger jsonrpc.FunctionLogger,
	buildPath, buildSketchCpp, dataFolder *paths.Path,
	connectionClosedCB func(),
	inoLanguageServer *INOLanguageServer,
) *ClangdLSPClient {
	clangdStdout, clangdStdin, clangdStderr := startClangd(logger, buildPath, buildSketchCpp, dataFolder)
	clangdStdio := streams.NewReadWriteCloser(clangdStdin, clangdStdout)
	if enableLogging {
		clangdStdio = streams.LogReadWriteCloserAs(clangdStdio, "inols-clangd.log")
		go io.Copy(streams.OpenLogFileAs("inols-clangd-err.log"), clangdStderr)
	} else {
		go io.Copy(os.Stderr, clangdStderr)
	}

	client := &ClangdLSPClient{
		ls: inoLanguageServer,
	}
	client.conn = lsp.NewClient(clangdStdio, clangdStdio, client)
	client.conn.SetLogger(&LSPLogger{IncomingPrefix: "IDE     LS <-- Clangd", OutgoingPrefix: "IDE     LS --> Clangd"})
	go func() {
		defer streams.CatchAndLogPanic()
		client.conn.Run()
		connectionClosedCB()
	}()

	return client
}

func startClangd(logger jsonrpc.FunctionLogger, compileCommandsDir, sketchCpp, dataFolder *paths.Path) (io.WriteCloser, io.ReadCloser, io.ReadCloser) {
	// Start clangd
	args := []string{
		globalClangdPath,
		"-log=verbose",
		fmt.Sprintf(`--compile-commands-dir=%s`, compileCommandsDir),
	}
	if dataFolder != nil {
		args = append(args, fmt.Sprintf("-query-driver=%s", dataFolder.Join("packages", "**")))
	}
	logger.Logf("    Starting clangd: %s", strings.Join(args, " "))
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

func (client *ClangdLSPClient) Close() {
	panic("unimplemented")
}

// The following are events incoming from Clangd

func (client *ClangdLSPClient) WindowShowMessageRequest(context.Context, jsonrpc.FunctionLogger, *lsp.ShowMessageRequestParams) (*lsp.MessageActionItem, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (client *ClangdLSPClient) WindowShowDocument(context.Context, jsonrpc.FunctionLogger, *lsp.ShowDocumentParams) (*lsp.ShowDocumentResult, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (client *ClangdLSPClient) WindowWorkDoneProgressCreate(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.WorkDoneProgressCreateParams) *jsonrpc.ResponseError {
	return client.ls.WindowWorkDoneProgressCreateFromClangd(ctx, logger, params)
}

func (client *ClangdLSPClient) ClientRegisterCapability(context.Context, jsonrpc.FunctionLogger, *lsp.RegistrationParams) *jsonrpc.ResponseError {
	panic("unimplemented")
}

func (client *ClangdLSPClient) ClientUnregisterCapability(context.Context, jsonrpc.FunctionLogger, *lsp.UnregistrationParams) *jsonrpc.ResponseError {
	panic("unimplemented")
}

func (client *ClangdLSPClient) WorkspaceWorkspaceFolders(context.Context, jsonrpc.FunctionLogger) ([]lsp.WorkspaceFolder, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (client *ClangdLSPClient) WorkspaceConfiguration(context.Context, jsonrpc.FunctionLogger, *lsp.ConfigurationParams) ([]json.RawMessage, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (client *ClangdLSPClient) WorkspaceApplyEdit(context.Context, jsonrpc.FunctionLogger, *lsp.ApplyWorkspaceEditParams) (*lsp.ApplyWorkspaceEditResult, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (client *ClangdLSPClient) WorkspaceCodeLensRefresh(context.Context, jsonrpc.FunctionLogger) *jsonrpc.ResponseError {
	panic("unimplemented")
}

func (client *ClangdLSPClient) Progress(logger jsonrpc.FunctionLogger, progress *lsp.ProgressParams) {
	client.ls.ProgressFromClangd(logger, progress)
}

func (client *ClangdLSPClient) LogTrace(jsonrpc.FunctionLogger, *lsp.LogTraceParams) {
	panic("unimplemented")
}

func (client *ClangdLSPClient) WindowShowMessage(jsonrpc.FunctionLogger, *lsp.ShowMessageParams) {
	panic("unimplemented")
}

func (client *ClangdLSPClient) WindowLogMessage(jsonrpc.FunctionLogger, *lsp.LogMessageParams) {
	panic("unimplemented")
}

func (client *ClangdLSPClient) TelemetryEvent(jsonrpc.FunctionLogger, json.RawMessage) {
	panic("unimplemented")
}

func (client *ClangdLSPClient) TextDocumentPublishDiagnostics(logger jsonrpc.FunctionLogger, params *lsp.PublishDiagnosticsParams) {
	client.ls.PublishDiagnosticsFromClangd(logger, params)
}
