package main

import (
	"io"
)

// StreamReadWrite combines ReadCloser and WriteCloser to ReadWriteCloser with logging.
type StreamReadWrite struct {
	inStream  io.ReadCloser
	outStream io.WriteCloser
	inLog     io.Writer
	outLog    io.Writer
}

func (srw StreamReadWrite) Read(p []byte) (int, error) {
	n, err := srw.inStream.Read(p)
	if n > 0 && srw.inLog != nil {
		srw.inLog.Write(p[:n])
	}
	return n, err
}

func (srw StreamReadWrite) Write(p []byte) (int, error) {
	if srw.outLog != nil {
		srw.outLog.Write(p)
	}
	return srw.outStream.Write(p)
}

func (srw StreamReadWrite) Close() error {
	err1 := srw.inStream.Close()
	err2 := srw.outStream.Close()
	if err1 != nil {
		return err1
	}
	return err2
}
