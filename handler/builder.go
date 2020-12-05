package handler

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/arduino/arduino-cli/arduino/libraries"
	"github.com/arduino/arduino-cli/executils"
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
		handler.synchronizer.DataMux.Lock()
		handler.initializeWorkbench(nil)
		handler.synchronizer.DataMux.Unlock()
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
	for uri, trackedFile := range handler.trackedFiles {
		rel, err := uri.AsPath().RelFrom(handler.sketchRoot)
		if err != nil {
			return nil, errors.WithMessage(err, "dumping tracked files")
		}
		data.Overrides[rel.String()] = trackedFile.Text
	}
	var overridesJSON string
	if jsonBytes, err := json.MarshalIndent(data, "", "  "); err != nil {
		return nil, errors.WithMessage(err, "dumping tracked files")
	} else if tmpFile, err := paths.WriteToTempFile(jsonBytes, nil, ""); err != nil {
		return nil, errors.WithMessage(err, "dumping tracked files")
	} else {
		overridesJSON = tmpFile.String()
	}

	// XXX: do this from IDE or via gRPC
	args := []string{globalCliPath,
		"compile",
		"--fqbn", fqbn,
		"--only-compilation-database",
		"--clean",
		"--source-override", overridesJSON,
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
