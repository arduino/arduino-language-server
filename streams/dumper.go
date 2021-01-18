package streams

import (
	"fmt"
	"io"
	"log"
	"os"

	"github.com/arduino/go-paths-helper"
)

// GlobalLogDirectory is the directory where logs are created
var GlobalLogDirectory *paths.Path

// LogReadWriteCloserAs return a proxy for the given upstream io.ReadWriteCloser
// that forward and logs all read/write/close operations on the given filename
// that is created in the GlobalLogDirectory.
func LogReadWriteCloserAs(upstream io.ReadWriteCloser, filename string) io.ReadWriteCloser {
	return &dumper{
		upstream: upstream,
		logfile:  OpenLogFileAs(filename),
	}
}

// LogReadWriteCloserToFile return a proxy for the given upstream io.ReadWriteCloser
// that forward and logs all read/write/close operations on the given file.
func LogReadWriteCloserToFile(upstream io.ReadWriteCloser, file *os.File) io.ReadWriteCloser {
	return &dumper{
		upstream: upstream,
		logfile:  file,
	}
}

// OpenLogFileAs creates a log file in GlobalLogDirectory.
func OpenLogFileAs(filename string) *os.File {
	path := GlobalLogDirectory.Join(filename)
	res, err := path.Append()
	if err != nil {
		log.Fatalf("Error opening log file: %s", err)
	} else {
		abs, _ := path.Abs()
		log.Printf("logging to %s", abs)
	}
	res.WriteString("\n\n\n\n\n\n\nStarted logging.\n")
	return res
}

type dumper struct {
	upstream io.ReadWriteCloser
	logfile  *os.File
	reading  bool
	writing  bool
}

func (d *dumper) Read(buff []byte) (int, error) {
	n, err := d.upstream.Read(buff)
	if err != nil {
		d.logfile.Write([]byte(fmt.Sprintf("<<< Read Error: %s\n", err)))
	} else {
		if !d.reading {
			d.reading = true
			d.writing = false
			d.logfile.Write([]byte("\n<<<\n"))
		}
		d.logfile.Write(buff[:n])
	}
	return n, err
}

func (d *dumper) Write(buff []byte) (int, error) {
	n, err := d.upstream.Write(buff)
	if err != nil {
		_, _ = d.logfile.Write([]byte(fmt.Sprintf(">>> Write Error: %s\n", err)))
	} else {
		if !d.writing {
			d.writing = true
			d.reading = false
			d.logfile.Write([]byte("\n>>>\n"))
		}
		_, _ = d.logfile.Write(buff[:n])
	}
	return n, err
}

func (d *dumper) Close() error {
	err := d.upstream.Close()
	_, _ = d.logfile.Write([]byte(fmt.Sprintf("--- Stream closed, err=%s\n", err)))
	_ = d.logfile.Close()
	return err
}
