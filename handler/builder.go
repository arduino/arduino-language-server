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
	"github.com/arduino/go-paths-helper"
	"github.com/bcmi-labs/arduino-language-server/lsp"
	"github.com/bcmi-labs/arduino-language-server/streams"
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
			{
				// Request a new progress token
				req := &lsp.WorkDoneProgressCreateParams{Token: "arduinoLanguageServerRebuild"}
				var resp lsp.WorkDoneProgressCreateResult
				if err := handler.StdioConn.Call(context.Background(), "window/workDoneProgress/create", req, &resp, nil); err != nil {
					log.Printf("    !!! could not create report progress: %s", err)
					<-done
					return
				}
			}

			req := &lsp.ProgressParams{Token: "arduinoLanguageServerRebuild"}
			req.Value = lsp.WorkDoneProgressBegin{
				Title: "Building sketch",
			}
			if err := handler.StdioConn.Notify(context.Background(), "$/progress", req, nil); err != nil {
				log.Printf("    !!! could not report progress: %s", err)
			}
			count := 0
			dots := []string{".", "..", "..."}
			for {
				select {
				case <-time.After(time.Millisecond * 400):
					msg := "compiling" + dots[count%3]
					count++
					req.Value = lsp.WorkDoneProgressReport{Message: &msg}
					if err := handler.StdioConn.Notify(context.Background(), "$/progress", req, nil); err != nil {
						log.Printf("    !!! could not report progress: %s", err)
					}
				case <-done:
					msg := "done"
					req.Value = lsp.WorkDoneProgressEnd{Message: &msg}
					if err := handler.StdioConn.Notify(context.Background(), "$/progress", req, nil); err != nil {
						log.Printf("    !!! could not report progress: %s", err)
					}
					return
				}
			}
		}()

		handler.dataMux.Lock()
		handler.initializeWorkbench(nil)
		handler.dataMux.Unlock()
		done <- true
		close(done)
	}
}

func (handler *InoHandler) generateBuildEnvironment() (*paths.Path, error) {
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
			return nil, errors.WithMessage(err, "dumping tracked files")
		}
		data.Overrides[rel.String()] = trackedFile.Text
	}
	var overridesJSON *paths.Path
	if jsonBytes, err := json.MarshalIndent(data, "", "  "); err != nil {
		return nil, errors.WithMessage(err, "dumping tracked files")
	} else if tmpFile, err := paths.WriteToTempFile(jsonBytes, nil, ""); err != nil {
		return nil, errors.WithMessage(err, "dumping tracked files")
	} else {
		overridesJSON = tmpFile
		defer tmpFile.Remove()
	}

	// XXX: do this from IDE or via gRPC
	args := []string{globalCliPath,
		"compile",
		"--fqbn", fqbn,
		"--only-compilation-database",
		"--clean",
		"--source-override", overridesJSON.String(),
		"--format", "json",
		sketchDir.String(),
	}
	cmd, err := executils.NewProcess(args...)
	if err != nil {
		return nil, errors.Errorf("running %s: %s", strings.Join(args, " "), err)
	}
	cmdOutput := &bytes.Buffer{}
	cmd.RedirectStdoutTo(cmdOutput)
	cmd.SetDirFromPath(sketchDir)
	log.Println("running: ", strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		return nil, errors.Errorf("running %s: %s", strings.Join(args, " "), err)
	}

	type cmdBuilderRes struct {
		BuildPath     *paths.Path `json:"build_path"`
		UsedLibraries []*libraries.Library
	}
	type cmdRes struct {
		CompilerOut   string        `json:"compiler_out"`
		CompilerErr   string        `json:"compiler_err"`
		BuilderResult cmdBuilderRes `json:"builder_result"`
	}
	var res cmdRes
	if err := json.Unmarshal(cmdOutput.Bytes(), &res); err != nil {
		return nil, errors.Errorf("parsing arduino-cli output: %s", err)
	}

	// Return only the build path
	log.Println("arduino-cli output:", cmdOutput)
	return res.BuilderResult.BuildPath, nil
}
