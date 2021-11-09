package ls

import (
	"bytes"
	"runtime"
	"strings"
	"time"

	"github.com/arduino/arduino-cli/arduino/builder"
	"github.com/arduino/arduino-cli/arduino/libraries"
	"github.com/arduino/arduino-cli/executils"
	"github.com/arduino/arduino-language-server/streams"
	"github.com/arduino/go-paths-helper"
	"github.com/fatih/color"
	"github.com/pkg/errors"
	"go.bug.st/json"
	"go.bug.st/lsp"
	"go.bug.st/lsp/jsonrpc"
)

func (handler *INOLanguageServer) scheduleRebuildEnvironment() {
	handler.rebuildSketchDeadlineMutex.Lock()
	defer handler.rebuildSketchDeadlineMutex.Unlock()
	d := time.Now().Add(time.Second)
	handler.rebuildSketchDeadline = &d
}

func (handler *INOLanguageServer) rebuildEnvironmentLoop() {
	logger := NewLSPFunctionLogger(color.HiMagentaString, "RBLD---")

	grabDeadline := func() *time.Time {
		handler.rebuildSketchDeadlineMutex.Lock()
		defer handler.rebuildSketchDeadlineMutex.Unlock()

		res := handler.rebuildSketchDeadline
		handler.rebuildSketchDeadline = nil
		return res
	}

	for {
		// Wait for someone to schedule a preprocessing...
		time.Sleep(100 * time.Millisecond)
		deadline := grabDeadline()
		if deadline == nil {
			continue
		}

		for time.Now().Before(*deadline) {
			time.Sleep(100 * time.Millisecond)

			if d := grabDeadline(); d != nil {
				deadline = d
			}
		}

		// Regenerate preprocessed sketch!
		done := make(chan bool)
		go func() {
			defer streams.CatchAndLogPanic()

			handler.progressHandler.Create("arduinoLanguageServerRebuild")
			handler.progressHandler.Begin("arduinoLanguageServerRebuild", &lsp.WorkDoneProgressBegin{
				Title: "Building sketch",
			})

			count := 0
			dots := []string{".", "..", "..."}
			for {
				select {
				case <-time.After(time.Millisecond * 400):
					msg := "compiling" + dots[count%3]
					count++
					handler.progressHandler.Report("arduinoLanguageServerRebuild", &lsp.WorkDoneProgressReport{Message: msg})
				case <-done:
					msg := "done"
					handler.progressHandler.End("arduinoLanguageServerRebuild", &lsp.WorkDoneProgressEnd{Message: msg})
					return
				}
			}
		}()

		handler.writeLock(logger, false)
		handler.initializeWorkbench(logger, nil)
		handler.writeUnlock(logger)
		done <- true
		close(done)
	}
}

func (ls *INOLanguageServer) generateBuildEnvironment(logger jsonrpc.FunctionLogger) error {
	sketchDir := ls.sketchRoot
	fqbn := ls.config.SelectedBoard.Fqbn

	// Export temporary files
	type overridesFile struct {
		Overrides map[string]string `json:"overrides"`
	}
	data := overridesFile{Overrides: map[string]string{}}
	for uri, trackedFile := range ls.trackedInoDocs {
		rel, err := paths.New(uri).RelFrom(ls.sketchRoot)
		if err != nil {
			return errors.WithMessage(err, "dumping tracked files")
		}
		data.Overrides[rel.String()] = trackedFile.Text
	}
	var overridesJSON *paths.Path
	if jsonBytes, err := json.MarshalIndent(data, "", "  "); err != nil {
		return errors.WithMessage(err, "dumping tracked files")
	} else if tmp, err := paths.WriteToTempFile(jsonBytes, nil, ""); err != nil {
		return errors.WithMessage(err, "dumping tracked files")
	} else {
		logger.Logf("Dumped overrides: %s", string(jsonBytes))
		overridesJSON = tmp
		defer tmp.Remove()
	}

	// XXX: do this from IDE or via gRPC
	args := []string{globalCliPath,
		"--config-file", globalCliConfigPath,
		"compile",
		"--fqbn", fqbn,
		"--only-compilation-database",
		"--clean",
		"--source-override", overridesJSON.String(),
		"--build-path", ls.buildPath.String(),
		"--format", "json",
		sketchDir.String(),
	}
	cmd, err := executils.NewProcess(args...)
	if err != nil {
		return errors.Errorf("running %s: %s", strings.Join(args, " "), err)
	}
	cmdOutput := &bytes.Buffer{}
	cmd.RedirectStdoutTo(cmdOutput)
	cmd.SetDirFromPath(sketchDir)
	logger.Logf("running: %s", strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		return errors.Errorf("running %s: %s", strings.Join(args, " "), err)
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
		return errors.Errorf("parsing arduino-cli output: %s", err)
	}
	logger.Logf("arduino-cli output: %s", cmdOutput)

	// TODO: do canonicalization directly in `arduino-cli`
	canonicalizeCompileCommandsJSON(ls.buildPath)

	return nil
}

func canonicalizeCompileCommandsJSON(compileCommandsDir *paths.Path) {
	compileCommandsJSONPath := compileCommandsDir.Join("compile_commands.json")
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
