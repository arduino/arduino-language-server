package ls

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/arduino/arduino-cli/arduino/builder"
	"github.com/arduino/arduino-cli/arduino/libraries"
	"github.com/arduino/arduino-cli/executils"
	rpc "github.com/arduino/arduino-cli/rpc/cc/arduino/cli/commands/v1"
	"github.com/arduino/arduino-language-server/sourcemapper"
	"github.com/arduino/arduino-language-server/streams"
	"github.com/arduino/go-paths-helper"
	"github.com/fatih/color"
	"github.com/pkg/errors"
	"go.bug.st/json"
	"go.bug.st/lsp"
	"go.bug.st/lsp/jsonrpc"
	"google.golang.org/grpc"
)

type SketchRebuilder struct {
	ls      *INOLanguageServer
	trigger chan bool
	cancel  func()
	mutex   sync.Mutex
}

func NewSketchBuilder(ls *INOLanguageServer) *SketchRebuilder {
	res := &SketchRebuilder{
		trigger: make(chan bool, 1),
		cancel:  func() {},
		ls:      ls,
	}
	go func() {
		defer streams.CatchAndLogPanic()
		res.rebuilderLoop()
	}()
	return res
}

func (ls *INOLanguageServer) triggerRebuild() {
	ls.sketchRebuilder.TriggerRebuild()
}

func (r *SketchRebuilder) TriggerRebuild() {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.cancel() // Stop possibly already running builds
	select {
	case r.trigger <- true:
	default:
	}
}

func (r *SketchRebuilder) rebuilderLoop() {
	logger := NewLSPFunctionLogger(color.HiMagentaString, "SKETCH REBUILD: ")
	for {
		<-r.trigger

		for {
			// Concede a 200ms delay to accumulate bursts of changes
			select {
			case <-r.trigger:
				continue
			case <-time.After(time.Second):
			}
			break
		}

		r.ls.progressHandler.Create("arduinoLanguageServerRebuild")
		r.ls.progressHandler.Begin("arduinoLanguageServerRebuild", &lsp.WorkDoneProgressBegin{Title: "Building sketch"})

		ctx, cancel := context.WithCancel(context.Background())
		r.mutex.Lock()
		logger.Logf("Sketch rebuild started")
		r.cancel = cancel
		r.mutex.Unlock()

		if err := r.doRebuild(ctx, logger); err != nil {
			logger.Logf("Error: %s", err)
		}

		cancel()
		r.ls.progressHandler.End("arduinoLanguageServerRebuild", &lsp.WorkDoneProgressEnd{Message: "done"})
	}
}

func (r *SketchRebuilder) doRebuild(ctx context.Context, logger jsonrpc.FunctionLogger) error {
	ls := r.ls

	if success, err := ls.generateBuildEnvironment(ctx, logger); err != nil {
		return err
	} else if !success {
		return fmt.Errorf("build failed")
	}

	ls.writeLock(logger, true)
	defer ls.writeUnlock(logger)

	// Check one last time if the process has been canceled
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	if err := ls.buildPath.Join("compile_commands.json").CopyTo(ls.compileCommandsDir.Join("compile_commands.json")); err != nil {
		logger.Logf("ERROR: updating compile_commands: %s", err)
	}

	if cppContent, err := ls.buildSketchCpp.ReadFile(); err == nil {
		oldVesrion := ls.sketchMapper.CppText.Version
		ls.sketchMapper = sourcemapper.CreateInoMapper(cppContent)
		ls.sketchMapper.CppText.Version = oldVesrion + 1
		ls.sketchMapper.DebugLogAll()
	} else {
		return errors.WithMessage(err, "reading generated cpp file from sketch")
	}

	// Send didSave to notify clang that the source cpp is changed
	logger.Logf("Sending 'didSave' notification to Clangd")
	cppURI := lsp.NewDocumentURIFromPath(ls.buildSketchCpp)
	didSaveParams := &lsp.DidSaveTextDocumentParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: cppURI},
	}
	if err := ls.Clangd.conn.TextDocumentDidSave(didSaveParams); err != nil {
		logger.Logf("error reinitilizing clangd:", err)
		return err
	}

	// Send the full text to clang
	logger.Logf("Sending full-text 'didChange' notification to Clangd")
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
		logger.Logf("error reinitilizing clangd:", err)
		return err
	}

	return nil
}

func (ls *INOLanguageServer) generateBuildEnvironment(ctx context.Context, logger jsonrpc.FunctionLogger) (bool, error) {
	// Extract all build information from language server status
	ls.readLock(logger, false)
	sketchRoot := ls.sketchRoot
	buildPath := ls.buildPath
	config := ls.config
	type overridesFile struct {
		Overrides map[string]string `json:"overrides"`
	}
	data := overridesFile{Overrides: map[string]string{}}
	for uri, trackedFile := range ls.trackedInoDocs {
		rel, err := paths.New(uri).RelFrom(sketchRoot)
		if err != nil {
			ls.readUnlock(logger)
			return false, errors.WithMessage(err, "dumping tracked files")
		}
		data.Overrides[rel.String()] = trackedFile.Text
	}
	ls.readUnlock(logger)

	var success bool
	if config.CliPath == nil {
		// Establish a connection with the arduino-cli gRPC server
		conn, err := grpc.Dial(config.CliDaemonAddress, grpc.WithInsecure(), grpc.WithBlock())
		if err != nil {
			return false, fmt.Errorf("error connecting to arduino-cli rpc server: %w", err)
		}
		defer conn.Close()
		client := rpc.NewArduinoCoreServiceClient(conn)

		compileReq := &rpc.CompileRequest{
			Instance:                      &rpc.Instance{Id: int32(config.CliInstanceNumber)},
			Fqbn:                          config.Fqbn,
			SketchPath:                    sketchRoot.String(),
			SourceOverride:                data.Overrides,
			BuildPath:                     buildPath.String(),
			CreateCompilationDatabaseOnly: true,
			Verbose:                       true,
		}
		compileReqJson, _ := json.MarshalIndent(compileReq, "", "  ")
		logger.Logf("Running build with: %s", string(compileReqJson))

		compRespStream, err := client.Compile(context.Background(), compileReq)
		if err != nil {
			return false, fmt.Errorf("error running compile: %w", err)
		}

		// Loop and consume the server stream until all the operations are done.
		stdout := ""
		stderr := ""
		for {
			compResp, err := compRespStream.Recv()
			if err == io.EOF {
				success = true
				logger.Logf("Compile successful!")
				break
			}
			if err != nil {
				logger.Logf("build stdout:")
				logger.Logf(stdout)
				logger.Logf("build stderr:")
				logger.Logf(stderr)
				return false, fmt.Errorf("error running compile: %w", err)
			}

			if resp := compResp.GetOutStream(); resp != nil {
				stdout += string(resp)
			}
			if resperr := compResp.GetErrStream(); resperr != nil {
				stderr += string(resperr)
			}
		}

	} else {

		// Dump overrides into a temporary json file
		for filename, override := range data.Overrides {
			logger.Logf("Dumping %s override:\n%s", filename, override)
		}
		var overridesJSON *paths.Path
		if jsonBytes, err := json.MarshalIndent(data, "", "  "); err != nil {
			return false, errors.WithMessage(err, "dumping tracked files")
		} else if tmp, err := paths.WriteToTempFile(jsonBytes, nil, ""); err != nil {
			return false, errors.WithMessage(err, "dumping tracked files")
		} else {
			overridesJSON = tmp
			defer tmp.Remove()
		}

		// Run arduino-cli to perform the build
		args := []string{config.CliPath.String(),
			"--config-file", config.CliConfigPath.String(),
			"compile",
			"--fqbn", config.Fqbn,
			"--only-compilation-database",
			"--source-override", overridesJSON.String(),
			"--build-path", buildPath.String(),
			"--format", "json",
			//"--clean",
			sketchRoot.String(),
		}
		cmd, err := executils.NewProcess(args...)
		if err != nil {
			return false, errors.Errorf("running %s: %s", strings.Join(args, " "), err)
		}
		cmdOutput := &bytes.Buffer{}
		cmd.RedirectStdoutTo(cmdOutput)
		cmd.SetDirFromPath(sketchRoot)
		logger.Logf("running: %s", strings.Join(args, " "))
		if err := cmd.RunWithinContext(ctx); err != nil {
			return false, errors.Errorf("running %s: %s", strings.Join(args, " "), err)
		}

		// Currently those values are not used, keeping here for future improvements
		type cmdBuilderRes struct {
			BuildPath     *paths.Path `json:"build_path"`
			UsedLibraries []*libraries.Library
		}
		type cmdRes struct {
			CompilerOut   string        `json:"compiler_out"`
			CompilerErr   string        `json:"compiler_err"`
			BuilderResult cmdBuilderRes `json:"builder_result"`
			Success       bool          `json:"success"`
		}
		var res cmdRes
		if err := json.Unmarshal(cmdOutput.Bytes(), &res); err != nil {
			return false, errors.Errorf("parsing arduino-cli output: %s", err)
		}
		logger.Logf("arduino-cli output: %s", cmdOutput)
		success = res.Success
	}

	// TODO: do canonicalization directly in `arduino-cli`
	canonicalizeCompileCommandsJSON(buildPath.Join("compile_commands.json"))

	return success, nil
}

func canonicalizeCompileCommandsJSON(compileCommandsJSONPath *paths.Path) {
	compileCommands, err := builder.LoadCompilationDatabase(compileCommandsJSONPath)
	if err != nil {
		panic("could not find compile_commands.json")
	}
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
	}

	// Save back compile_commands.json with OS native file separator and extension
	compileCommands.SaveToFile()
}
