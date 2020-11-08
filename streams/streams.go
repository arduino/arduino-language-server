package streams

import "io"

// NewReadWriteCloser create an io.ReadWriteCloser from given io.ReadCloser and io.WriteCloser.
func NewReadWriteCloser(in io.ReadCloser, out io.WriteCloser) io.ReadWriteCloser {
	return &combinedReadWriteCloser{in, out}
}

type combinedReadWriteCloser struct {
	reader io.ReadCloser
	writer io.WriteCloser
}

func (sd *combinedReadWriteCloser) Read(p []byte) (int, error) {
	return sd.reader.Read(p)
}

func (sd *combinedReadWriteCloser) Write(p []byte) (int, error) {
	return sd.writer.Write(p)
}

func (sd *combinedReadWriteCloser) Close() error {
	ierr := sd.reader.Close()
	oerr := sd.writer.Close()
	if ierr != nil {
		return ierr
	}
	if oerr != nil {
		return oerr
	}
	return nil
}
