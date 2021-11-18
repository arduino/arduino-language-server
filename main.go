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

func main() {
	clangdPath := flag.String(
		"clangd", "",
		"Path to clangd executable")
	cliPath := flag.String(
		"cli", "",
		"Path to arduino-cli executable")
	cliConfigPath := flag.String(
		"cli-config", "",
		"Path to arduino-cli config file")
	fqbn := flag.String(
		"fqbn", "",
		"Fully qualified board name to use initially (can be changed via JSON-RPC)")
	/* unused */ _ = flag.String(
		"board-name", "",
		"User-friendly board name to use initially (can be changed via JSON-RPC)")
	enableLogging := flag.Bool(
		"log", false,
		"Enable logging to files")
	loggingBasePath := flag.String(
		"logpath", ".",
		"Location where to write logging files to when logging is enabled")
	formatFilePath := flag.String(
		"format-conf-path", "",
		"Path to global clang-format configuration file")
	cliDaemonAddress := flag.String(
		"cli-daemon-addr", "",
		"TCP address and port of the Arduino CLI daemon (for example: localhost:50051)")
	cliDaemonInstanceNumber := flag.Int(
		"cli-daemon-instance", -1,
		"Instance number of the Arduino CLI daemon")
	flag.Parse()

	if *loggingBasePath != "" {
		streams.GlobalLogDirectory = paths.New(*loggingBasePath)
	} else if *enableLogging {
		log.Fatalf("Please specify logpath")
	}

	if *enableLogging {
		logfile := streams.OpenLogFileAs("inols-err.log")
		log.SetOutput(io.MultiWriter(logfile, os.Stderr))
		defer streams.CatchAndLogPanic()
		go func() {
			log.Println(http.ListenAndServe("localhost:6060", nil))
		}()
		log.Println("Language server launched with arguments:")
		for i, arg := range os.Args {
			log.Printf("  arg[%d] = %s", i, arg)
		}
	} else {
		log.SetOutput(os.Stderr)
	}

	if *cliPath != "" {
		if *cliConfigPath == "" {
			log.Fatal("Path to ArduinoCLI config file must be set.")
		}
	} else if *cliDaemonAddress == "" || *cliDaemonInstanceNumber == -1 {
		log.Fatal("ArduinoCLI daemon address and instance number must be set.")
	}
	if *clangdPath == "" {
		log.Fatal("Path to Clangd must be set.")
	}

	config := &ls.Config{
		Fqbn:              *fqbn,
		ClangdPath:        paths.New(*clangdPath),
		EnableLogging:     *enableLogging,
		CliPath:           paths.New(*cliPath),
		CliConfigPath:     paths.New(*cliConfigPath),
		FormatterConf:     paths.New(*formatFilePath),
		CliDaemonAddress:  *cliDaemonAddress,
		CliInstanceNumber: *cliDaemonInstanceNumber,
	}

	stdio := streams.NewReadWriteCloser(os.Stdin, os.Stdout)
	if *enableLogging {
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
