package handler

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
)

// StreamLogger maintains log files for all streams involved in the language server
type StreamLogger struct {
	Default   io.WriteCloser
	Stdin     io.WriteCloser
	Stdout    io.WriteCloser
	ClangdIn  io.WriteCloser
	ClangdOut io.WriteCloser
	ClangdErr io.WriteCloser
}

// Close closes all logging streams
func (s *StreamLogger) Close() (err error) {
	var errs []string
	for _, c := range []io.Closer{s.Default, s.Stdin, s.Stdout, s.ClangdIn, s.ClangdOut, s.ClangdErr} {
		if c == nil {
			continue
		}

		err = c.Close()
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) != 0 {
		return fmt.Errorf(strings.Join(errs, ", "))
	}

	return nil
}

// AttachStdInOut attaches the stdin, stdout logger to the in/out channels
func (s *StreamLogger) AttachStdInOut(in io.ReadCloser, out io.WriteCloser) io.ReadWriteCloser {
	return &streamDuplex{
		io.TeeReader(in, s.Stdin),
		in,
		io.MultiWriter(out, s.Stdout),
		out,
	}
}

// AttachClangdInOut attaches the clangd in, out logger to the in/out channels
func (s *StreamLogger) AttachClangdInOut(in io.ReadCloser, out io.WriteCloser) io.ReadWriteCloser {
	return &streamDuplex{
		io.TeeReader(in, s.ClangdIn),
		in,
		io.MultiWriter(out, s.ClangdOut),
		out,
	}
}

type streamDuplex struct {
	in   io.Reader
	inc  io.Closer
	out  io.Writer
	outc io.Closer
}

func (sd *streamDuplex) Read(p []byte) (int, error) {
	return sd.in.Read(p)
}

func (sd *streamDuplex) Write(p []byte) (int, error) {
	return sd.out.Write(p)
}

func (sd *streamDuplex) Close() error {
	ierr := sd.inc.Close()
	oerr := sd.outc.Close()

	if ierr != nil {
		return ierr
	}
	if oerr != nil {
		return oerr
	}
	return nil
}

// NewStreamLogger creates files for all stream logs. Returns an error if opening a single stream fails.
func NewStreamLogger(basepath string) (res *StreamLogger, err error) {
	res = &StreamLogger{}

	res.Default, err = os.OpenFile(filepath.Join(basepath, "inols.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		res.Close()
		return
	}
	res.Stdin, err = os.OpenFile(filepath.Join(basepath, "inols-stdin.log"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		res.Close()
		return
	}
	res.Stdout, err = os.OpenFile(filepath.Join(basepath, "inols-stdout.log"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		res.Close()
		return
	}
	res.ClangdIn, err = os.OpenFile(filepath.Join(basepath, "inols-clangd-in.log"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		res.Close()
		return
	}
	res.ClangdOut, err = os.OpenFile(filepath.Join(basepath, "inols-clangd-out.log"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		res.Close()
		return
	}
	res.ClangdErr, err = os.OpenFile(filepath.Join(basepath, "inols-clangd-err.log"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		res.Close()
		return
	}

	return
}

// NewNoopLogger creates a logger that does nothing
func NewNoopLogger() (res *StreamLogger) {
	noop := noopCloser{ioutil.Discard}
	return &StreamLogger{
		Default:   noop,
		Stdin:     noop,
		Stdout:    noop,
		ClangdIn:  noop,
		ClangdOut: noop,
		ClangdErr: noop,
	}
}

type noopCloser struct {
	io.Writer
}

func (noopCloser) Close() error {
	return nil
}
