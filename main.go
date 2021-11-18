package main

import (
	"flag"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"

	"github.com/arduino/arduino-language-server/ls"
	"github.com/arduino/arduino-language-server/streams"
	"github.com/arduino/go-paths-helper"
)

var clangdPath string
var compileCommandsDir string
var cliPath string
var cliConfigPath string
var initialFqbn string
var initialBoardName string
var enableLogging bool
var loggingBasePath string
var formatFilePath string

func main() {
	flag.StringVar(&clangdPath, "clangd", "clangd", "Path to clangd executable")
	flag.StringVar(&compileCommandsDir, "compile-commands-dir", "", "Specify a path to look for compile_commands.json. If path is invalid, clangd will look in the current directory and parent paths of each source file. If not specified, the clangd process is started without the compilation database.")
	flag.StringVar(&cliPath, "cli", "arduino-cli", "Path to arduino-cli executable")
	flag.StringVar(&cliConfigPath, "cli-config", "", "Path to arduino-cli config file")
	flag.StringVar(&initialFqbn, "fqbn", "arduino:avr:uno", "Fully qualified board name to use initially (can be changed via JSON-RPC)")
	flag.StringVar(&initialBoardName, "board-name", "", "User-friendly board name to use initially (can be changed via JSON-RPC)")
	flag.BoolVar(&enableLogging, "log", false, "Enable logging to files")
	flag.StringVar(&loggingBasePath, "logpath", ".", "Location where to write logging files to when logging is enabled")
	flag.StringVar(&formatFilePath, "format-conf-path", "", "Path to global clang-format configuration file")
	flag.Parse()

	if loggingBasePath != "" {
		streams.GlobalLogDirectory = paths.New(loggingBasePath)
	} else if enableLogging {
		log.Fatalf("Please specify logpath")
	}

	if enableLogging {
		logfile := streams.OpenLogFileAs("inols-err.log")
		log.SetOutput(io.MultiWriter(logfile, os.Stderr))
		defer streams.CatchAndLogPanic()
		go func() {
			log.Println(http.ListenAndServe("localhost:6060", nil))
		}()
	} else {
		log.SetOutput(os.Stderr)
	}

	if cliConfigPath == "" {
		log.Fatal("Path to ArduinoCLI config file must be set.")
	}
	if clangdPath == "" {
		log.Fatal("Path to Clangd must be set.")
	}

	config := &ls.Config{
		Fqbn:          initialFqbn,
		ClangdPath:    paths.New(clangdPath),
		EnableLogging: enableLogging,
	}
	if cliPath != "" {
		config.CliPath = paths.New(cliPath)
	}
	if cliConfigPath != "" {
		config.CliConfigPath = paths.New(cliConfigPath)
	}
	if formatFilePath != "" {
		config.FormatterConf = paths.New(formatFilePath)
	}

	stdio := streams.NewReadWriteCloser(os.Stdin, os.Stdout)
	if enableLogging {
		stdio = streams.LogReadWriteCloserAs(stdio, "inols.log")
	}

	inoHandler := ls.NewINOLanguageServer(stdio, stdio, config)

	// Intercept kill signal
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, os.Kill)

	select {
	case <-inoHandler.CloseNotify():
	case <-c:
		log.Println("INTERRUPTED")
	}
	inoHandler.CleanUp()
	inoHandler.Close()
}
