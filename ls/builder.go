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
	"strings"
	"sync"
	"time"

	"github.com/arduino/arduino-cli/arduino/libraries"
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

type sketchRebuilder struct {
	ls      *INOLanguageServer
	trigger chan chan<- bool
	cancel  func()
	mutex   sync.Mutex
}

// newSketchBuilder makes a new SketchRebuilder and returns its pointer
func newSketchBuilder(ls *INOLanguageServer) *sketchRebuilder {
	res := &sketchRebuilder{
		trigger: make(chan chan<- bool, 1),
		cancel:  func() {},
		ls:      ls,
	}
	go func() {
		defer streams.CatchAndLogPanic()
		res.rebuilderLoop()
	}()
	return res
}

func (ls *INOLanguageServer) triggerRebuildAndWait(logger jsonrpc.FunctionLogger) {
	completed := make(chan bool)
	ls.sketchRebuilder.TriggerRebuild(completed)
	ls.writeUnlock(logger)
	<-completed
	ls.writeLock(logger, true)
}

func (ls *INOLanguageServer) triggerRebuild() {
	ls.sketchRebuilder.TriggerRebuild(nil)
}

// TriggerRebuild schedule a sketch rebuild (it will be executed asynchronously)
func (r *sketchRebuilder) TriggerRebuild(completed chan<- bool) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.cancel() // Stop possibly already running builds
	select {
	case r.trigger <- completed:
	default:
	}
}

func (r *sketchRebuilder) rebuilderLoop() {
	logger := NewLSPFunctionLogger(color.HiMagentaString, "SKETCH REBUILD: ")
	for {
		completed := <-r.trigger

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

		if err := r.doRebuildArduinoPreprocessedSketch(ctx, logger); err != nil {
			logger.Logf("Error: %s", err)
		}

		cancel()
		r.ls.progressHandler.End("arduinoLanguageServerRebuild", &lsp.WorkDoneProgressEnd{Message: "done"})
		if completed != nil {
			close(completed)
		}
	}
}

func (r *sketchRebuilder) doRebuildArduinoPreprocessedSketch(ctx context.Context, logger jsonrpc.FunctionLogger) error {
	ls := r.ls
	if success, err := ls.generateBuildEnvironment(ctx, !r.ls.config.SkipLibrariesDiscoveryOnRebuild, logger); err != nil {
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

	if cppContent, err := ls.buildSketchCpp.ReadFile(); err == nil {
		oldVersion := ls.sketchMapper.CppText.Version
		ls.sketchMapper = sourcemapper.CreateInoMapper(cppContent)
		ls.sketchMapper.CppText.Version = oldVersion + 1
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
		logger.Logf("error reinitializing clangd:", err)
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
		logger.Logf("error reinitializing clangd:", err)
		return err
	}

	return nil
}

func (ls *INOLanguageServer) generateBuildEnvironment(ctx context.Context, fullBuild bool, logger jsonrpc.FunctionLogger) (bool, error) {
	var buildPath *paths.Path
	if fullBuild {
		buildPath = ls.fullBuildPath
	} else {
		buildPath = ls.buildPath
	}

	// Extract all build information from language server status
	ls.readLock(logger, false)
	sketchRoot := ls.sketchRoot
	config := ls.config
	type overridesFile struct {
		Overrides map[string]string `json:"overrides"`
	}
	data := overridesFile{Overrides: map[string]string{}}
	for uri, trackedFile := range ls.trackedIdeDocs {
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
			SkipLibrariesDiscovery:        !fullBuild,
		}
		compileReqJSON, _ := json.MarshalIndent(compileReq, "", "  ")
		logger.Logf("Running build with: %s", string(compileReqJSON))

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
		args := []string{
			"--config-file", config.CliConfigPath.String(),
			"compile",
			"--fqbn", config.Fqbn,
			"--only-compilation-database",
			"--source-override", overridesJSON.String(),
			"--build-path", buildPath.String(),
			"--format", "json",
		}
		if !fullBuild {
			args = append(args, "--skip-libraries-discovery")
		}
		args = append(args, sketchRoot.String())

		cmd, err := paths.NewProcessFromPath(nil, config.CliPath, args...)
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

	if fullBuild {
		ls.CopyFullBuildResults(logger, buildPath)
		return ls.generateBuildEnvironment(ctx, false, logger)
	}

	// TODO: do canonicalization directly in `arduino-cli`
	canonicalizeCompileCommandsJSON(buildPath.Join("compile_commands.json"))

	return success, nil
}
