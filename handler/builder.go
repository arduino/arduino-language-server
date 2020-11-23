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

func filterErrorsAndWarnings(cppCode []byte) string {
	var sb strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(cppCode))
	for scanner.Scan() {
		lineStr := scanner.Text()
		if !(strings.HasPrefix(lineStr, "ERROR:") || strings.HasPrefix(lineStr, "WARNING:")) {
			sb.WriteString(lineStr)
			sb.WriteRune('\n')
		}
	}
	return sb.String()
}

func copyIno2Cpp(inoCode string, cppPath string) (cppCode []byte, err error) {
	inoPath := strings.TrimSuffix(cppPath, ".cpp")
	filePrefix := "#include <Arduino.h>\n#line 1 \"" + inoPath + "\"\n"
	cppCode = []byte(filePrefix + inoCode)
	err = ioutil.WriteFile(cppPath, cppCode, 0600)
	if err != nil {
		err = errors.Wrap(err, "Error while writing target file to temporary directory.")
		return
	}
	if enableLogging {
		log.Println("Target file written to", cppPath)
	}
	return
}

func printCompileFlags(buildProps *properties.Map, printer *Printer, fqbn string) {
	if strings.Contains(fqbn, ":avr:") {
		printer.Println("--target=avr")
	} else if strings.Contains(fqbn, ":sam:") {
		printer.Println("--target=arm-none-eabi")
	}
	cppFlags := buildProps.ExpandPropsInString(buildProps.Get("compiler.cpp.flags"))
	printer.Println(splitFlags(cppFlags))
	mcu := buildProps.ExpandPropsInString(buildProps.Get("build.mcu"))
	if strings.Contains(fqbn, ":avr:") {
		printer.Println("-mmcu=", mcu)
	} else if strings.Contains(fqbn, ":sam:") {
		printer.Println("-mcpu=", mcu)
	}
	fcpu := buildProps.ExpandPropsInString(buildProps.Get("build.f_cpu"))
	printer.Println("-DF_CPU=", fcpu)
	ideVersion := buildProps.ExpandPropsInString(buildProps.Get("runtime.ide.version"))
	printer.Println("-DARDUINO=", ideVersion)
	board := buildProps.ExpandPropsInString(buildProps.Get("build.board"))
	printer.Println("-DARDUINO_", board)
	arch := buildProps.ExpandPropsInString(buildProps.Get("build.arch"))
	printer.Println("-DARDUINO_ARCH_", arch)
	if strings.Contains(fqbn, ":sam:") {
		libSamFlags := buildProps.ExpandPropsInString(buildProps.Get("compiler.libsam.c.flags"))
		printer.Println(splitFlags(libSamFlags))
	}
	extraFlags := buildProps.ExpandPropsInString(buildProps.Get("build.extra_flags"))
	printer.Println(splitFlags(extraFlags))
	corePath := buildProps.ExpandPropsInString(buildProps.Get("build.core.path"))
	printer.Println("-I", corePath)
	variantPath := buildProps.ExpandPropsInString(buildProps.Get("build.variant.path"))
	printer.Println("-I", variantPath)
	if strings.Contains(fqbn, ":avr:") {
		avrgccPath := buildProps.ExpandPropsInString(buildProps.Get("runtime.tools.avr-gcc.path"))
		printer.Println("-I", filepath.Join(avrgccPath, "avr", "include"))
	}

	printLibraryPaths(corePath, printer)
}

func printLibraryPaths(basePath string, printer *Printer) {
	parentDir := filepath.Dir(basePath)
	if strings.HasSuffix(parentDir, string(filepath.Separator)) || strings.HasSuffix(parentDir, ".") {
		return
	}
	libsDir := filepath.Join(parentDir, "libraries")
	if libraries, err := ioutil.ReadDir(libsDir); err == nil {
		for _, libInfo := range libraries {
			if libInfo.IsDir() {
				srcDir := filepath.Join(libsDir, libInfo.Name(), "src")
				if srcInfo, err := os.Stat(srcDir); err == nil && srcInfo.IsDir() {
					printer.Println("-I", srcDir)
				} else {
					printer.Println("-I", filepath.Join(libsDir, libInfo.Name()))
				}
			}
		}
	}
	printLibraryPaths(parentDir, printer)
}

// Printer prints to a Writer and stores the first error.
type Printer struct {
	Writer *bufio.Writer
	Err    error
}

// Println prints the given strings followed by a line break.
func (printer *Printer) Println(text ...string) {
	totalLen := 0
	for i := range text {
		if len(text[i]) > 0 {
			_, err := printer.Writer.WriteString(text[i])
			if err != nil && printer.Err == nil {
				printer.Err = err
			}
			totalLen += len(text[i])
		}
	}
	if totalLen > 0 {
		_, err := printer.Writer.WriteString("\n")
		if err != nil && printer.Err == nil {
			printer.Err = err
		}
	}
}

// Flush flushes the underlying writer.
func (printer *Printer) Flush() {
	err := printer.Writer.Flush()
	if err != nil && printer.Err == nil {
		printer.Err = err
	}
}

func splitFlags(flags string) string {
	flagsBytes := []byte(flags)
	result := make([]byte, len(flagsBytes))
	inSingleQuotes := false
	inDoubleQuotes := false
	for i, b := range flagsBytes {
		if b == '\'' && !inDoubleQuotes {
			inSingleQuotes = !inSingleQuotes
		}
		if b == '"' && !inSingleQuotes {
			inDoubleQuotes = !inDoubleQuotes
		}
		if b == ' ' && !inSingleQuotes && !inDoubleQuotes {
			result[i] = '\n'
		} else {
			result[i] = b
		}
	}
	return string(result)
}

func logCommandErr(command *exec.Cmd, stdout []byte, err error, filter func(string) string) error {
	message := ""
	log.Println("Command error:", command.Args, err)
	if len(stdout) > 0 {
		stdoutStr := string(stdout)
		log.Println("------------------------------BEGIN STDOUT\n", stdoutStr, "------------------------------END STDOUT")
		message += filter(stdoutStr)
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		stderr := exitErr.Stderr
		if len(stderr) > 0 {
			stderrStr := string(stderr)
			log.Println("------------------------------BEGIN STDERR\n", stderrStr, "------------------------------END STDERR")
			message += filter(stderrStr)
		}
	}
	if len(message) == 0 {
		return err
	}
	return errors.New(message)
}

func errMsgFilter(tempDir string) func(string) string {
	if !strings.HasSuffix(tempDir, string(filepath.Separator)) {
		tempDir += string(filepath.Separator)
	}
	return func(s string) string {
		return strings.ReplaceAll(s, tempDir, "")
	}
}
