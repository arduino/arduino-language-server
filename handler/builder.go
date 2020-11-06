package handler

import (
	"bufio"
	"bytes"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/arduino/go-properties-orderedmap"
	"github.com/pkg/errors"
)

func generateCpp(inoCode []byte, sourcePath, fqbn string) (cppPath string, cppCode []byte, err error) {
	// The CLI expects the `theSketchName.ino` file to be in `some/path/theSketchName` folder.
	// Expected folder structure: `/path/to/temp/ino2cpp-${random}/theSketchName/theSketchName.ino`.
	rawRootTempDir, err := ioutil.TempDir("", "ino2cpp-")
	if err != nil {
		err = errors.Wrap(err, "Error while creating temporary directory.")
		return
	}
	rootTempDir, err := filepath.EvalSymlinks(rawRootTempDir)
	if err != nil {
		err = errors.Wrap(err, "Error while resolving symbolic links of temporary directory.")
		return
	}

	sketchName := filepath.Base(sourcePath)
	if strings.HasSuffix(sketchName, ".ino") {
		sketchName = sketchName[:len(sketchName)-len(".ino")]
	}
	sketchTempPath := filepath.Join(rootTempDir, sketchName)
	createDirIfNotExist(sketchTempPath)

	// Write source file to temp dir
	sketchFileName := sketchName + ".ino"
	inoPath := filepath.Join(sketchTempPath, sketchFileName)
	err = ioutil.WriteFile(inoPath, inoCode, 0600)
	if err != nil {
		err = errors.Wrap(err, "Error while writing source file to temporary directory.")
		return
	}
	if enableLogging {
		log.Println("Source file written to", inoPath)
	}

	// Copy all header files to temp dir
	err = copyHeaderFiles(filepath.Dir(sourcePath), rootTempDir)
	if err != nil {
		return
	}

	// Generate compile_flags.txt
	cppPath = filepath.Join(sketchTempPath, sketchFileName+".cpp")
	flagsPath, err := generateCompileFlags(sketchTempPath, inoPath, sourcePath, fqbn)
	if err != nil {
		return
	}
	if enableLogging {
		log.Println("Compile flags written to", flagsPath)
	}

	// Generate target file
	cppCode, err = generateTargetFile(sketchTempPath, inoPath, cppPath, fqbn)
	return
}

func createDirIfNotExist(dir string) {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		err = os.MkdirAll(dir, os.ModePerm)
		if err != nil {
			panic(err)
		}
	}
}

func copyHeaderFiles(sourceDir string, destDir string) error {
	fileInfos, err := ioutil.ReadDir(sourceDir)
	if err != nil {
		return err
	}
	for _, fileInfo := range fileInfos {
		if !fileInfo.IsDir() && strings.HasSuffix(fileInfo.Name(), ".h") {
			input, err := ioutil.ReadFile(filepath.Join(sourceDir, fileInfo.Name()))
			if err != nil {
				return err
			}

			err = ioutil.WriteFile(filepath.Join(destDir, fileInfo.Name()), input, 0644)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func updateCpp(inoCode []byte, sourcePath, fqbn string, fqbnChanged bool, cppPath string) (cppCode []byte, err error) {
	tempDir := filepath.Dir(cppPath)
	inoPath := strings.TrimSuffix(cppPath, ".cpp")
	if inoCode != nil {
		// Write source file to temp dir
		err = ioutil.WriteFile(inoPath, inoCode, 0600)
		if err != nil {
			err = errors.Wrap(err, "Error while writing source file to temporary directory.")
			return
		}
		if enableLogging {
			log.Println("Source file written to", inoPath)
		}
	}

	if fqbnChanged {
		// Generate compile_flags.txt
		var flagsPath string
		flagsPath, err = generateCompileFlags(tempDir, inoPath, sourcePath, fqbn)
		if err != nil {
			return
		}
		if enableLogging {
			log.Println("Compile flags written to", flagsPath)
		}
	}

	// Generate target file
	cppCode, err = generateTargetFile(tempDir, inoPath, cppPath, fqbn)
	return
}

func generateCompileFlags(tempDir, inoPath, sourcePath, fqbn string) (string, error) {
	var cliArgs []string
	if len(fqbn) > 0 {
		cliArgs = []string{"compile", "--fqbn", fqbn, "--show-properties", inoPath}
	} else {
		cliArgs = []string{"compile", "--show-properties", inoPath}
	}
	propertiesCmd := exec.Command(globalCliPath, cliArgs...)
	output, err := propertiesCmd.Output()
	if err != nil {
		err = logCommandErr(globalCliPath, output, err, errMsgFilter(tempDir))
		return "", err
	}
	buildProps, err := properties.LoadFromBytes(output)
	if err != nil {
		return "", errors.Wrap(err, "Error while reading build properties.")
	}
	flagsPath := filepath.Join(tempDir, "compile_flags.txt")
	outFile, err := os.OpenFile(flagsPath, os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return flagsPath, errors.Wrap(err, "Error while creating output file for compile flags.")
	}
	defer outFile.Close()

	printer := Printer{Writer: bufio.NewWriter(outFile)}
	printCompileFlags(buildProps, &printer, fqbn)
	printLibraryPaths(sourcePath, &printer)
	printer.Flush()
	return flagsPath, printer.Err
}

func generateTargetFile(tempDir, inoPath, cppPath, fqbn string) (cppCode []byte, err error) {
	var cliArgs []string
	if len(fqbn) > 0 {
		cliArgs = []string{"compile", "--fqbn", fqbn, "--preprocess", inoPath}
	} else {
		cliArgs = []string{"compile", "--preprocess", inoPath}
	}
	preprocessCmd := exec.Command(globalCliPath, cliArgs...)
	cppCode, err = preprocessCmd.Output()
	if err != nil {
		err = logCommandErr(globalCliPath, cppCode, err, errMsgFilter(tempDir))
		return
	}

	// Filter lines beginning with ERROR or WARNING
	cppCode = []byte(filterErrorsAndWarnings(cppCode))

	err = ioutil.WriteFile(cppPath, cppCode, 0600)
	if err != nil {
		err = errors.Wrap(err, "Error while writing target file to temporary directory.")
	} else if enableLogging {
		log.Println("Target file written to", cppPath)
	}
	return
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

func logCommandErr(command string, stdout []byte, err error, filter func(string) string) error {
	message := ""
	log.Println("Command error:", command, err)
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
