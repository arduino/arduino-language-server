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
		args = append(args, fmt.Sprintf("-query-driver=%s", dataFolder.Join("packages", "**")))
	}

	logger.Logf("    Starting clangd: %s", strings.Join(args, " "))
	var clangdStdin io.WriteCloser
	var clangdStdout, clangdStderr io.ReadCloser
	if clangdCmd, err := executils.NewProcess(args...); err != nil {
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
	client.conn.SetLogger(&LSPLogger{
		IncomingPrefix: "IDE     LS <-- Clangd",
		OutgoingPrefix: "IDE     LS --> Clangd",
		HiColor:        color.HiRedString,
		LoColor:        color.RedString,
		ErrorColor:     color.New(color.BgHiMagenta, color.FgHiWhite, color.BlinkSlow).Sprintf,
	})
	return client
}

func (client *ClangdLSPClient) Run() {
	client.conn.Run()
}

func (client *ClangdLSPClient) Close() {
	client.conn.Exit() // send "exit" notification to Clangd
	// TODO: kill client.conn
}

// The following are events incoming from Clangd

func (client *ClangdLSPClient) WindowShowMessageRequest(context.Context, jsonrpc.FunctionLogger, *lsp.ShowMessageRequestParams) (*lsp.MessageActionItem, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (client *ClangdLSPClient) WindowShowDocument(context.Context, jsonrpc.FunctionLogger, *lsp.ShowDocumentParams) (*lsp.ShowDocumentResult, *jsonrpc.ResponseError) {
	panic("unimplemented")
}

func (client *ClangdLSPClient) WindowWorkDoneProgressCreate(ctx context.Context, logger jsonrpc.FunctionLogger, params *lsp.WorkDoneProgressCreateParams) *jsonrpc.ResponseError {
	return client.ls.WindowWorkDoneProgressCreateReqFromClangd(ctx, logger, params)
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
	client.ls.ProgressNotifFromClangd(logger, progress)
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
	go client.ls.PublishDiagnosticsNotifFromClangd(logger, params)
}
