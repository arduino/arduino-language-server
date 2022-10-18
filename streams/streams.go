// This file is part of arduino-language-server.
//
// Copyright 2022 ARDUINO SA (http://www.arduino.cc/)
//
// This software is released under the GNU Affero General Public License version 3,
// which covers the main part of arduino-language-server.
// The terms of this license can be found at:
// https://www.gnu.org/licenses/agpl-3.0.html
//
// You can be released from the requirements of the above licenses by purchasing
// a commercial license. Buying such a license is mandatory if you want to
// modify or otherwise use the software for commercial activities involving the
// Arduino software without disclosing the source code of your own applications.
// To purchase a commercial license, send an email to license@arduino.cc.

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
