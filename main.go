package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"

	"github.com/bcmi-labs/arduino-language-server/handler"
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

	// var stdinLog, stdoutLog, clangdinLog, clangdoutLog, clangderrLog io.Writer
	var logStreams *handler.StreamLogger
	if enableLogging {
		var err error
		logStreams, err = handler.NewStreamLogger(loggingBasePath)
		if err != nil {
			log.Fatal(err)
		}
		defer logStreams.Close()

		log.SetOutput(logStreams.Default)
	} else {
		logStreams = handler.NewNoopLogger()
		log.SetOutput(os.Stderr)
	}

	handler.Setup(cliPath, enableLogging, true)
	initialBoard := handler.Board{Fqbn: initialFqbn, Name: initialBoardName}
	inoHandler := handler.NewInoHandler(os.Stdin, os.Stdout, logStreams, startClangd, initialBoard)
	defer inoHandler.StopClangd()
	<-inoHandler.StdioConn.DisconnectNotify()
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
