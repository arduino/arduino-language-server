package streams

import (
	"fmt"
	"io"
	"os"
)

// LogReadWriteCloserToFile return a proxy for the given upstream io.ReadWriteCloser
// that forward and logs all read/write/close operations on the given file.
func LogReadWriteCloserToFile(upstream io.ReadWriteCloser, file *os.File) io.ReadWriteCloser {
	return &dumper{upstream, file}
}

type dumper struct {
	upstream io.ReadWriteCloser
	logfile  *os.File
}

func (d *dumper) Read(buff []byte) (int, error) {
	n, err := d.upstream.Read(buff)
	if err != nil {
		d.logfile.Write([]byte(fmt.Sprintf("<<< Read Error: %s\n", err)))
	} else {
		d.logfile.Write([]byte(fmt.Sprintf("<<< Read %d bytes:\n%s\n", n, buff[:n])))
	}
	return n, err
}

func (d *dumper) Write(buff []byte) (int, error) {
	n, err := d.upstream.Write(buff)
	if err != nil {
		_, _ = d.logfile.Write([]byte(fmt.Sprintf(">>> Write Error: %s\n", err)))
	} else {
		_, _ = d.logfile.Write([]byte(fmt.Sprintf(">>> Wrote %d bytes:\n%s\n", n, buff[:n])))
	}
	return n, err
}

func (d *dumper) Close() error {
	err := d.upstream.Close()
	_, _ = d.logfile.Write([]byte(fmt.Sprintf("--- Stream closed, err=%s\n", err)))
	_ = d.logfile.Close()
	return err
}
