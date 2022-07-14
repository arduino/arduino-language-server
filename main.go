package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path"

	"github.com/arduino/arduino-language-server/ls"
	"github.com/arduino/arduino-language-server/streams"
	"github.com/arduino/go-paths-helper"
	"github.com/mattn/go-isatty"
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
	skipLibrariesDiscoveryOnRebuild := flag.Bool(
		"skip-libraries-discovery-on-rebuild", false,
		"Skip libraries discovery on rebuild, it will make rebuilds faster but it will fail if the used libraries changes.")
	noRealTimeDiagnostics := flag.Bool(
		"no-real-time-diagnostics", false,
		"Disable real time diagnostics")
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

	if *cliDaemonAddress != "" || *cliDaemonInstanceNumber != -1 {
		// if one is set, both must be set
		if *cliDaemonAddress == "" || *cliDaemonInstanceNumber == -1 {
			log.Fatal("ArduinoCLI daemon address and instance number must be set.")
		}
	} else {
		if *cliConfigPath == "" {
			if user, _ := user.Current(); user != nil {
				candidate := path.Join(user.HomeDir, ".arduino15/arduino-cli.yaml")
				if _, err := os.Stat(candidate); err == nil {
					*cliConfigPath = candidate
					log.Printf("ArduinoCLI config file found at %s\n", candidate)
				}
			}
		}
		if *cliConfigPath == "" {
			log.Fatal("Path to ArduinoCLI config file must be set.")
		}
		if *cliPath == "" {
			bin, _ := exec.LookPath("arduino-cli")
			if bin == "" {
				log.Fatal("Path to ArduinoCLI must be set.")
			}
			log.Printf("arduino-cli found at %s\n", bin)
			*cliPath = bin
		}
	}

	if *clangdPath == "" {
		bin, _ := exec.LookPath("clangd")
		if bin == "" {
			log.Fatal("Path to Clangd must be set.")
		}
		log.Printf("clangd found at %s\n", bin)
		*clangdPath = bin
	}

	config := &ls.Config{
		Fqbn:                            *fqbn,
		ClangdPath:                      paths.New(*clangdPath),
		EnableLogging:                   *enableLogging,
		CliPath:                         paths.New(*cliPath),
		CliConfigPath:                   paths.New(*cliConfigPath),
		FormatterConf:                   paths.New(*formatFilePath),
		CliDaemonAddress:                *cliDaemonAddress,
		CliInstanceNumber:               *cliDaemonInstanceNumber,
		SkipLibrariesDiscoveryOnRebuild: *skipLibrariesDiscoveryOnRebuild,
		DisableRealTimeDiagnostics:      *noRealTimeDiagnostics,
	}

	stdio := streams.NewReadWriteCloser(os.Stdin, os.Stdout)
	if *enableLogging {
		stdio = streams.LogReadWriteCloserAs(stdio, "inols.log")
	}

	inoHandler := ls.NewINOLanguageServer(stdio, stdio, config)

	if isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd()) {
		fmt.Fprint(os.Stderr, `
arduino-language-server is a language server that provides IDE-like features to editors.

It should be used via an editor plugin rather than invoked directly. For more information, see:
https://github.com/arduino/arduino-language-server/
https://microsoft.github.io/language-server-protocol/
`)
	}

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
