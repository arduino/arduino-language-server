package main

import (
	"context"
	"flag"
	"io"
	"log"
	"os"
	"os/exec"
	"os/user"
	"strings"

	"github.com/sourcegraph/jsonrpc2"
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

	inoHandler := newInoHandler()
	clangdStream := jsonrpc2.NewBufferedStream(StreamReadWrite{clangdIn, clangdOut, clangdinLog, clangdoutLog}, jsonrpc2.VSCodeObjectCodec{})
	clangdHandler := jsonrpc2.HandlerWithError(inoHandler.FromClangd)
	inoHandler.clangdConn = jsonrpc2.NewConn(context.Background(), clangdStream, clangdHandler)
	stdStream := jsonrpc2.NewBufferedStream(StreamReadWrite{os.Stdin, os.Stdout, stdinLog, stdoutLog}, jsonrpc2.VSCodeObjectCodec{})
	stdHandler := jsonrpc2.HandlerWithError(inoHandler.FromStdio)
	inoHandler.stdioConn = jsonrpc2.NewConn(context.Background(), stdStream, stdHandler)

	<-inoHandler.stdioConn.DisconnectNotify()
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
