package main

import (
	"flag"
	"log"
	"os"
	"syscall"

	"github.com/arduino/go-paths-helper"
	"github.com/bcmi-labs/arduino-language-server/handler"
	"github.com/bcmi-labs/arduino-language-server/lsp"
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

	if loggingBasePath != "" {
		streams.GlobalLogDirectory = paths.New(loggingBasePath)
	} else if enableLogging {
		log.Fatalf("Please specify logpath")
	}

	if enableLogging {
		logfile := streams.OpenLogFileAs("inols-err.log")
		defer func() {
			// In case of panic output the stack trace in the log file before exiting
			if r := recover(); r != nil {
				log.Println(string(debug.Stack()))
			}
			logfile.Close()
		}()
		log.SetOutput(io.MultiWriter(logfile, os.Stderr))
		// log.SetOutput(logfile)
	} else {
		log.SetOutput(os.Stderr)
	}

	handler.Setup(cliPath, clangdPath, enableLogging, true)
	initialBoard := lsp.Board{Fqbn: initialFqbn, Name: initialBoardName}

	stdio := streams.NewReadWriteCloser(os.Stdin, os.Stdout)
	if enableLogging {
		stdio = streams.LogReadWriteCloserAs(stdio, "inols.log")
	}

	inoHandler := handler.NewInoHandler(stdio, initialBoard)
	defer inoHandler.StopClangd()
	<-inoHandler.StdioConn.DisconnectNotify()
}
