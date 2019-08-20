package main

import (
	"flag"
	"io"
	"log"
	"os"
	"os/exec"
	"os/user"
	"strings"

	"github.com/bcmi-labs/arduino-language-server/handler"
)

var enableLogging bool

func main() {
	flag.BoolVar(&enableLogging, "log", false, "enable logging to files")
	flag.Parse()

	var stdinLog, stdoutLog, clangdinLog, clangdoutLog, clangderrLog io.Writer
	if enableLogging {
		logFile, stdinLogFile, stdoutLogFile, clangdinLogFile, clangdoutLogFile, clangderrLogFile := createLogFiles()
		defer logFile.Close()
		defer stdinLogFile.Close()
		defer stdoutLogFile.Close()
		defer clangdinLogFile.Close()
		defer clangdoutLogFile.Close()
		defer clangderrLogFile.Close()
		log.SetOutput(logFile)
		log.Println("Starting clangd...")
		stdinLog, stdoutLog, clangdinLog, clangdoutLog, clangderrLog = stdinLogFile, stdoutLogFile, clangdinLogFile, clangdoutLogFile, clangderrLogFile
	} else {
		log.SetOutput(os.Stderr)
	}

	clangdIn, clangdOut, clangdErr := startClangd()
	defer clangdIn.Close()
	if enableLogging {
		go io.Copy(clangderrLog, clangdErr)
	}

	inoHandler := handler.NewInoHandler(os.Stdin, os.Stdout, stdinLog, stdoutLog, clangdIn, clangdOut, clangdinLog, clangdoutLog, enableLogging)
	<-inoHandler.StdioConn.DisconnectNotify()
}

func createLogFiles() (logFile, stdinLog, stdoutLog, clangdinLog, clangdoutLog, clangderrLog *os.File) {
	var err error
	logFile, err = os.OpenFile("inols.log", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		panic(err)
	}
	stdinLog, err = os.OpenFile("inols-stdin.log", os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		panic(err)
	}
	stdoutLog, err = os.OpenFile("inols-stdout.log", os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		panic(err)
	}
	clangdinLog, err = os.OpenFile("inols-clangd-in.log", os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		panic(err)
	}
	clangdoutLog, err = os.OpenFile("inols-clangd-out.log", os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		panic(err)
	}
	clangderrLog, err = os.OpenFile("inols-clangd-err.log", os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		panic(err)
	}
	return
}

const clangdExec = "clangd"

func startClangd() (clangdOut io.ReadCloser, clangdIn io.WriteCloser, clangdErr io.ReadCloser) {
	usr, err := user.Current()
	if err != nil {
		panic(err)
	}
	clangdCmd := exec.Command(strings.Replace(clangdExec, "~", usr.HomeDir, 1))
	clangdIn, _ = clangdCmd.StdinPipe()
	clangdOut, _ = clangdCmd.StdoutPipe()
	if enableLogging {
		clangdErr, _ = clangdCmd.StderrPipe()
	}
	err = clangdCmd.Start()
	if err != nil {
		panic(err)
	}
	return
}
