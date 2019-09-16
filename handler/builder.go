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

	"github.com/pkg/errors"
)

func generateCpp(inoCode []byte, name, fqbn string) (cppPath string, cppCode []byte, err error) {
	rawTempDir, err := ioutil.TempDir("", "ino2cpp-")
	if err != nil {
		err = errors.Wrap(err, "Error while creating temporary directory.")
		return
	}
	tempDir, err := filepath.EvalSymlinks(rawTempDir)
	if err != nil {
		err = errors.Wrap(err, "Error while resolving symbolic links of temporary directory.")
		return
	}

	// Write source file to temp dir
	if !strings.HasSuffix(name, ".ino") {
		name += ".ino"
	}
	inoPath := filepath.Join(tempDir, name)
	err = ioutil.WriteFile(inoPath, inoCode, 0600)
	if err != nil {
		err = errors.Wrap(err, "Error while writing source file to temporary directory.")
		return
	}
	if enableLogging {
		log.Println("Source file written to", inoPath)
	}

	// Generate compile_flags.txt
	flagsPath, err := generateCompileFlags(tempDir, inoPath, fqbn)
	if err != nil {
		return
	}
	if enableLogging {
		log.Println("Compile flags written to", flagsPath)
	}

	// Generate target file
	preprocessCmd := exec.Command(globalCliPath, "compile", "--fqbn", fqbn, "--preprocess", inoPath)
	cppCode, err = preprocessCmd.Output()
	if err != nil {
		err = logCommandErr(globalCliPath, cppCode, err, errMsgFilter(tempDir))
		return
	}

	// Write target file to temp dir
	cppPath = filepath.Join(tempDir, name+".cpp")
	err = ioutil.WriteFile(cppPath, cppCode, 0600)
	if err != nil {
		err = errors.Wrap(err, "Error while writing target file to temporary directory.")
	} else if enableLogging {
		log.Println("Target file written to", cppPath)
	}
	return
}

func updateCpp(inoCode []byte, fqbn, cppPath string) (cppCode []byte, err error) {
	// Write source file to temp dir
	inoPath := strings.TrimSuffix(cppPath, ".cpp")
	err = ioutil.WriteFile(inoPath, inoCode, 0600)
	if err != nil {
		err = errors.Wrap(err, "Error while writing source file to temporary directory.")
		return
	}

	// Generate target file
	preprocessCmd := exec.Command(globalCliPath, "compile", "--fqbn", fqbn, "--preprocess", inoPath)
	cppCode, err = preprocessCmd.Output()
	if err != nil {
		err = logCommandErr(globalCliPath, cppCode, err, errMsgFilter(filepath.Dir(inoPath)))
		return
	}

	// Write target file to temp dir
	err = ioutil.WriteFile(cppPath, cppCode, 0600)
	if err != nil {
		err = errors.Wrap(err, "Error while writing target file to temporary directory.")
	}
	return
}

func generateCompileFlags(tempDir, inoPath, fqbn string) (string, error) {
	propertiesCmd := exec.Command(globalCliPath, "compile", "--fqbn", fqbn, "--show-properties", inoPath)
	output, err := propertiesCmd.Output()
	if err != nil {
		err = logCommandErr(globalCliPath, output, err, errMsgFilter(tempDir))
		return "", err
	}
	properties, err := readProperties(bytes.NewReader(output))
	if err != nil {
		return "", errors.Wrap(err, "Error while reading build properties.")
	}
	flagsPath := filepath.Join(tempDir, "compile_flags.txt")
	outFile, err := os.OpenFile(flagsPath, os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return flagsPath, errors.Wrap(err, "Error while creating output file for compile flags.")
	}
	defer outFile.Close()
	writer := bufio.NewWriter(outFile)

	// TODO support other architectures
	writer.WriteString("--target=avr\n")
	cppFlags := expandProperty(properties, "compiler.cpp.flags")
	writer.WriteString(strings.ReplaceAll(cppFlags, " ", "\n") + "\n")
	mcu := expandProperty(properties, "build.mcu")
	writer.WriteString("-mmcu=" + mcu + "\n")
	fcpu := expandProperty(properties, "build.f_cpu")
	writer.WriteString("-DF_CPU=" + fcpu + "\n")
	ideVersion := expandProperty(properties, "runtime.ide.version")
	writer.WriteString("-DARDUINO=" + ideVersion + "\n")
	board := expandProperty(properties, "build.board")
	writer.WriteString("-DARDUINO_" + board + "\n")
	arch := expandProperty(properties, "build.arch")
	writer.WriteString("-DARDUINO_ARCH_" + arch + "\n")
	corePath := expandProperty(properties, "build.core.path")
	writer.WriteString("-I" + corePath + "\n")
	variantPath := expandProperty(properties, "build.variant.path")
	writer.WriteString("-I" + variantPath + "\n")
	avrgccPath := expandProperty(properties, "runtime.tools.avr-gcc.path")
	writer.WriteString("-I" + filepath.Join(avrgccPath, "avr", "include") + "\n")

	writer.Flush()
	return flagsPath, nil
}

func logCommandErr(command string, stdout []byte, err error, filter func(string) string) error {
	message := ""
	log.Println("Command error:", command, err)
	if len(stdout) > 0 {
		stdoutStr := string(stdout)
		log.Println("------------------------------BEGIN STDOUT\n", stdoutStr, "\n------------------------------END STDOUT")
		message += filter(stdoutStr)
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		stderr := exitErr.Stderr
		if len(stderr) > 0 {
			stderrStr := string(stderr)
			log.Println("------------------------------BEGIN STDERR\n", stderrStr, "\n------------------------------END STDERR")
			message += filter(stderrStr)
		}
	}
	return errors.Wrap(err, message)
}

func errMsgFilter(tempDir string) func(string) string {
	if !strings.HasSuffix(tempDir, string(filepath.Separator)) {
		tempDir += string(filepath.Separator)
	}
	return func(s string) string {
		return strings.ReplaceAll(s, tempDir, "")
	}
}
