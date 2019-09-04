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
)

func generateCpp(inoCode []byte, name, fqbn string) (cppPath string, cppCode []byte, err error) {
	rawTempDir, err := ioutil.TempDir("", "ino2cpp-")
	if err != nil {
		return
	}
	tempDir, err := filepath.EvalSymlinks(rawTempDir)
	if err != nil {
		return
	}

	// Write source file to temp dir
	if !strings.HasSuffix(name, ".ino") {
		name += ".ino"
	}
	inoPath := filepath.Join(tempDir, name)
	err = ioutil.WriteFile(inoPath, inoCode, 0600)
	if err != nil {
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
		logCommandErr(globalCliPath, cppCode, err)
		return
	}

	// Write target file to temp dir
	cppPath = filepath.Join(tempDir, name+".cpp")
	err = ioutil.WriteFile(cppPath, cppCode, 0600)
	if err == nil && enableLogging {
		log.Println("Target file written to", cppPath)
	}
	return
}

func updateCpp(inoCode []byte, fqbn, cppPath string) (cppCode []byte, err error) {
	// Write source file to temp dir
	inoPath := strings.TrimSuffix(cppPath, ".cpp")
	err = ioutil.WriteFile(inoPath, inoCode, 0600)
	if err != nil {
		return
	}

	// Generate target file
	preprocessCmd := exec.Command(globalCliPath, "compile", "--fqbn", fqbn, "--preprocess", inoPath)
	cppCode, err = preprocessCmd.Output()
	if err != nil {
		logCommandErr(globalCliPath, cppCode, err)
		return
	}

	// Write target file to temp dir
	err = ioutil.WriteFile(cppPath, cppCode, 0600)
	return
}

func generateCompileFlags(tempDir, inoPath, fqbn string) (string, error) {
	propertiesCmd := exec.Command(globalCliPath, "compile", "--fqbn", fqbn, "--show-properties", inoPath)
	output, err := propertiesCmd.Output()
	if err != nil {
		logCommandErr(globalCliPath, output, err)
		return "", err
	}
	properties, err := readProperties(bytes.NewReader(output))
	if err != nil {
		return "", err
	}
	flagsPath := filepath.Join(tempDir, "compile_flags.txt")
	outFile, err := os.OpenFile(flagsPath, os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return flagsPath, err
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

func logCommandErr(command string, stdout []byte, err error) {
	log.Println("Command error:", command, err)
	if len(stdout) > 0 {
		log.Println("------------------------------BEGIN STDOUT\n", string(stdout), "\n------------------------------END STDOUT")
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		stderr := exitErr.Stderr
		if len(stderr) > 0 {
			log.Println("------------------------------BEGIN STDERR\n", string(stderr), "\n------------------------------END STDERR")
		}
	}
}
