package handler

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/arduino/arduino-cli/arduino/libraries"
	"github.com/arduino/arduino-cli/executils"
	"github.com/arduino/go-paths-helper"
	"github.com/arduino/go-properties-orderedmap"
	"github.com/pkg/errors"
)

// func generateCpp(sourcePath, fqbn string) (*paths.Path, []byte, error) {
// 	// Generate target file
// 	if cppPath, err := generateBuildEnvironment(paths.New(sourcePath), fqbn); err != nil {
// 		return nil, nil, err
// 	} else if cppCode, err := cppPath.ReadFile(); err != nil {
// 		return nil, nil, err
// 	} else {
// 		return cppPath, cppCode, err
// 	}
// }

func updateCpp(inoCode []byte, sourcePath, fqbn string, fqbnChanged bool, cppPath string) (cppCode []byte, err error) {
	// 	tempDir := filepath.Dir(cppPath)
	// 	inoPath := strings.TrimSuffix(cppPath, ".cpp")
	// 	if inoCode != nil {
	// 		// Write source file to temp dir
	// 		err = ioutil.WriteFile(inoPath, inoCode, 0600)
	// 		if err != nil {
	// 			err = errors.Wrap(err, "Error while writing source file to temporary directory.")
	// 			return
	// 		}
	// 		if enableLogging {
	// 			log.Println("Source file written to", inoPath)
	// 		}
	// 	}

	// 	if fqbnChanged {
	// 		// Generate compile_flags.txt
	// 		var flagsPath string
	// 		flagsPath, err = generateCompileFlags(tempDir, inoPath, sourcePath, fqbn)
	// 		if err != nil {
	// 			return
	// 		}
	// 		if enableLogging {
	// 			log.Println("Compile flags written to", flagsPath)
	// 		}
	// 	}

	// 	// Generate target file
	// 	cppCode, err = generateTargetFile(tempDir, inoPath, cppPath, fqbn)
	return
}

func generateBuildEnvironment(sketchDir *paths.Path, fqbn string) (*paths.Path, error) {
	// XXX: do this from IDE or via gRPC
	args := []string{globalCliPath,
		"compile",
		"--fqbn", fqbn,
		"--only-compilation-database",
		"--clean",
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
