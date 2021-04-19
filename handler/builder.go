package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/arduino/arduino-cli/arduino/libraries"
	"github.com/arduino/arduino-cli/executils"
	"github.com/arduino/arduino-language-server/lsp"
	"github.com/arduino/arduino-language-server/streams"
	"github.com/arduino/go-paths-helper"
	"github.com/pkg/errors"
)

func (handler *InoHandler) scheduleRebuildEnvironment() {
	handler.rebuildSketchDeadlineMutex.Lock()
	defer handler.rebuildSketchDeadlineMutex.Unlock()
	d := time.Now().Add(time.Second)
	handler.rebuildSketchDeadline = &d
}

func (handler *InoHandler) rebuildEnvironmentLoop() {
	defer streams.CatchAndLogPanic()

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
					handler.progressHandler.Report("arduinoLanguageServerRebuild", &lsp.WorkDoneProgressReport{Message: &msg})
				case <-done:
					msg := "done"
					handler.progressHandler.End("arduinoLanguageServerRebuild", &lsp.WorkDoneProgressEnd{Message: &msg})
					return
				}
			}
		}()

		handler.dataLock("RBLD---")
		handler.initializeWorkbench(context.Background(), nil)
		handler.dataUnlock("RBLD---")
		done <- true
		close(done)
	}
}

func (handler *InoHandler) generateBuildEnvironment(buildPath *paths.Path) error {
	sketchDir := handler.sketchRoot
	fqbn := handler.config.SelectedBoard.Fqbn

	// Export temporary files
	type overridesFile struct {
		Overrides map[string]string `json:"overrides"`
	}
	data := overridesFile{Overrides: map[string]string{}}
	for uri, trackedFile := range handler.docs {
		rel, err := paths.New(uri).RelFrom(handler.sketchRoot)
		if err != nil {
			return errors.WithMessage(err, "dumping tracked files")
		}
		data.Overrides[rel.String()] = trackedFile.Text
	}
	var overridesJSON *paths.Path
	if jsonBytes, err := json.MarshalIndent(data, "", "  "); err != nil {
		return errors.WithMessage(err, "dumping tracked files")
	} else if tmpFile, err := paths.WriteToTempFile(jsonBytes, nil, ""); err != nil {
		return errors.WithMessage(err, "dumping tracked files")
	} else {
		overridesJSON = tmpFile
		defer tmpFile.Remove()
	}

	// XXX: do this from IDE or via gRPC
	args := []string{globalCliPath,
		"--config-file", globalCliConfigPath,
		"compile",
		"--fqbn", fqbn,
		"--only-compilation-database",
		"--clean",
		"--source-override", overridesJSON.String(),
		"--build-path", buildPath.String(),
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
	log.Println("running: ", strings.Join(args, " "))
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
	log.Println("arduino-cli output:", cmdOutput)

	return nil
}
