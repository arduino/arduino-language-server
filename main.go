package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/bcmi-labs/arduino-language-server/handler"
	"github.com/bcmi-labs/arduino-language-server/streams"
)

var clangdPath string
var compileCommandsDir string
var cliPath string
var initialFqbn string
var initialBoardName string
var enableLogging bool
var loggingBasePath string

func main() {
	flag.StringVar(&clangdPath, "clangd", "clangd", "Path to clangd executable")
	flag.StringVar(&compileCommandsDir, "compile-commands-dir", "", "Specify a path to look for compile_commands.json. If path is invalid, clangd will look in the current directory and parent paths of each source file. If not specified, the clangd process is started without the compilation database.")
	flag.StringVar(&cliPath, "cli", "arduino-cli", "Path to arduino-cli executable")
	flag.StringVar(&initialFqbn, "fqbn", "arduino:avr:uno", "Fully qualified board name to use initially (can be changed via JSON-RPC)")
	flag.StringVar(&initialBoardName, "board-name", "", "User-friendly board name to use initially (can be changed via JSON-RPC)")
	flag.BoolVar(&enableLogging, "log", false, "Enable logging to files")
	flag.StringVar(&loggingBasePath, "logpath", ".", "Location where to write logging files to when logging is enabled")
	flag.Parse()

	if enableLogging {
		logfile := openLogFile("inols-err.log")
		defer logfile.Close()
		log.SetOutput(logfile)
	} else {
		log.SetOutput(os.Stderr)
	}

	handler.Setup(cliPath, enableLogging, true)
	initialBoard := handler.Board{Fqbn: initialFqbn, Name: initialBoardName}

	clangdStdout, clangdStdin, clangdStderr := startClangd()
	clangdStdio := streams.NewReadWriteCloser(clangdStdin, clangdStdout)
	if enableLogging {
		logfile := openLogFile("inols-clangd.log")
		defer logfile.Close()
		clangdStdio = streams.LogReadWriteCloserToFile(clangdStdio, logfile)

		errLogfile := openLogFile("inols-clangd-err.log")
		defer errLogfile.Close()
		go io.Copy(errLogfile, clangdStderr)
	}

	stdio := streams.NewReadWriteCloser(os.Stdin, os.Stdout)
	if enableLogging {
		logfile := openLogFile("inols.log")
		defer logfile.Close()
		stdio = streams.LogReadWriteCloserToFile(stdio, logfile)
	}

	inoHandler := handler.NewInoHandler(stdio, clangdStdio, initialBoard)
	defer inoHandler.StopClangd()
	<-inoHandler.StdioConn.DisconnectNotify()
}

func openLogFile(name string) *os.File {
	path := filepath.Join(loggingBasePath, name)
	logfile, err := os.Create(path)
	if err != nil {
		log.Fatalf("Error opening log file: %s", err)
	} else {
		abs, _ := filepath.Abs(path)
		log.Println("logging to " + abs)
	}
	return logfile
}

func startClangd() (clangdIn io.WriteCloser, clangdOut io.ReadCloser, clangdErr io.ReadCloser) {
	if enableLogging {
		log.Println("Starting clangd process:", clangdPath)
	}
	var clangdCmd *exec.Cmd
	if compileCommandsDir != "" {
		clangdCmd = exec.Command(clangdPath)
	} else {
		clangdCmd = exec.Command(clangdPath, fmt.Sprintf(`--compile-commands-dir="%s"`, compileCommandsDir))
	}
	clangdIn, _ = clangdCmd.StdinPipe()
	clangdOut, _ = clangdCmd.StdoutPipe()
	clangdErr, _ = clangdCmd.StderrPipe()

	err := clangdCmd.Start()
	if err != nil {
		panic(err)
	}
	return
}
