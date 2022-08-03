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
	"github.com/fatih/color"
	"go.bug.st/json"
	"go.bug.st/lsp"
	"go.bug.st/lsp/jsonrpc"
)

type ClangdLSPClient struct {
	conn *lsp.Client
	ls   *INOLanguageServer
}

// NewClangdLSPClient creates and returns a new client
func NewClangdLSPClient(logger jsonrpc.FunctionLogger, dataFolder *paths.Path, ls *INOLanguageServer) *ClangdLSPClient {
	clangdConfFile := ls.buildPath.Join(".clangd")
	clangdConf := fmt.Sprintln("Diagnostics:")
	clangdConf += fmt.Sprintln("  Suppress: [anon_bitfield_qualifiers]")
	clangdConf += fmt.Sprintln("CompileFlags:")
	clangdConf += fmt.Sprintln("  Add: -ferror-limit=0")
	if err := clangdConfFile.WriteFile([]byte(clangdConf)); err != nil {
		logger.Logf("Error writing clangd configuration: %s", err)
	}

	// Start clangd
	args := []string{
		ls.config.ClangdPath.String(),
		"-log=verbose",
		fmt.Sprintf(`--compile-commands-dir=%s`, ls.buildPath),
	}
	if dataFolder != nil {
		args = append(args, fmt.Sprintf("-query-driver=%s", dataFolder.Join("packages", "**").Canonical()))
	}

	logger.Logf("    Starting clangd: %s", strings.Join(args, " "))
	var clangdStdin io.WriteCloser
	var clangdStdout, clangdStderr io.ReadCloser
	if clangdCmd, err := executils.NewProcess(nil, args...); err != nil {
		panic("starting clangd: " + err.Error())
	} else if cin, err := clangdCmd.StdinPipe(); err != nil {
		panic("getting clangd stdin: " + err.Error())
	} else if cout, err := clangdCmd.StdoutPipe(); err != nil {
		panic("getting clangd stdout: " + err.Error())
	} else if cerr, err := clangdCmd.StderrPipe(); err != nil {
		panic("getting clangd stderr: " + err.Error())
	} else if err := clangdCmd.Start(); err != nil {
		panic("running clangd: " + err.Error())
	} else {
		clangdStdin = cin
		clangdStdout = cout
		clangdStderr = cerr
	}

	clangdStdio := streams.NewReadWriteCloser(clangdStdout, clangdStdin)
	if ls.config.EnableLogging {
		clangdStdio = streams.LogReadWriteCloserAs(clangdStdio, "inols-clangd.log")
		go io.Copy(streams.OpenLogFileAs("inols-clangd-err.log"), clangdStderr)
	} else {
		go io.Copy(os.Stderr, clangdStderr)
	}

	client := &ClangdLSPClient{
		ls: ls,
	}
	client.conn = lsp.NewClient(clangdStdio, clangdStdio, client)
	client.conn.SetLogger(&Logger{
		IncomingPrefix: "IDE     LS <-- Clangd",
		OutgoingPrefix: "IDE     LS --> Clangd",
		HiColor:        color.HiRedString,
		LoColor:        color.RedString,
		ErrorColor:     color.New(color.BgHiMagenta, color.FgHiWhite, color.BlinkSlow).Sprintf,
	})
	return client
}

// Run sends a Run notification to Clangd
func (client *ClangdLSPClient) Run() {
	client.conn.Run()
}

// Close sends an Exit notification to Clangd
func (client *ClangdLSPClient) Close() {
	client.conn.Exit() // send "exit" notification to Clangd
	// TODO: kill client.conn
}

// The following are events incoming from Clangd

// WindowShowMessageRequest is not implemented
func (client *ClangdLSPClient) WindowShowMessageRequest(context.Context, jsonrpc.FunctionLogger, *lsp.ShowMessageRequestParams) (*lsp.MessageActionItem, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// WindowShowDocument is not implemented
func (client *ClangdLSPClient) WindowShowDocument(context.Context, jsonrpc.FunctionLogger, *lsp.ShowDocumentParams) (*lsp.ShowDocumentResult, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// WindowWorkDoneProgressCreate is not implemented
func (client *ClangdLSPClient) WindowWorkDoneProgressCreate(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.WorkDoneProgressCreateParams) *jsonrpc.ResponseError {
	return client.ls.WindowWorkDoneProgressCreateReqFromClangd(ctx, logger, params)
}

// ClientRegisterCapability is not implemented
func (client *ClangdLSPClient) ClientRegisterCapability(context.Context, jsonrpc.FunctionLogger, *lsp.RegistrationParams) *jsonrpc.ResponseError {
	panic("unimplemented")
}

// ClientUnregisterCapability is not implemented
func (client *ClangdLSPClient) ClientUnregisterCapability(context.Context, jsonrpc.FunctionLogger, *lsp.UnregistrationParams) *jsonrpc.ResponseError {
	panic("unimplemented")
}

// WorkspaceWorkspaceFolders is not implemented
func (client *ClangdLSPClient) WorkspaceWorkspaceFolders(context.Context, jsonrpc.FunctionLogger) ([]lsp.WorkspaceFolder, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// WorkspaceConfiguration is not implemented
func (client *ClangdLSPClient) WorkspaceConfiguration(context.Context, jsonrpc.FunctionLogger, *lsp.ConfigurationParams) ([]json.RawMessage, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// WorkspaceApplyEdit is not implemented
func (client *ClangdLSPClient) WorkspaceApplyEdit(context.Context, jsonrpc.FunctionLogger, *lsp.ApplyWorkspaceEditParams) (*lsp.ApplyWorkspaceEditResult, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

// WorkspaceCodeLensRefresh is not implemented
func (client *ClangdLSPClient) WorkspaceCodeLensRefresh(context.Context, jsonrpc.FunctionLogger) *jsonrpc.ResponseError {
	panic("unimplemented")
}

// Progress sends a Progress notification
func (client *ClangdLSPClient) Progress(logger jsonrpc.FunctionLogger, progress *lsp.ProgressParams) {
	client.ls.ProgressNotifFromClangd(logger, progress)
}

// LogTrace is not implemented
func (client *ClangdLSPClient) LogTrace(jsonrpc.FunctionLogger, *lsp.LogTraceParams) {
	panic("unimplemented")
}

// WindowShowMessage is not implemented
func (client *ClangdLSPClient) WindowShowMessage(jsonrpc.FunctionLogger, *lsp.ShowMessageParams) {
	panic("unimplemented")
}

// WindowLogMessage is not implemented
func (client *ClangdLSPClient) WindowLogMessage(jsonrpc.FunctionLogger, *lsp.LogMessageParams) {
	panic("unimplemented")
}

// TelemetryEvent is not implemented
func (client *ClangdLSPClient) TelemetryEvent(jsonrpc.FunctionLogger, json.RawMessage) {
	panic("unimplemented")
}

// TextDocumentPublishDiagnostics sends a notification to Publish Dignostics
func (client *ClangdLSPClient) TextDocumentPublishDiagnostics(logger jsonrpc.FunctionLogger, params *lsp.PublishDiagnosticsParams) {
	go client.ls.PublishDiagnosticsNotifFromClangd(logger, params)
}
