// This file is part of arduino-language-server.
//
// Copyright 2026 ARDUINO SA (http://www.arduino.cc/)
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

package ls

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInlayHintRequestReturnsNull(t *testing.T) {
	request := `{"jsonrpc":"2.0","id":1,"method":"textDocument/inlayHint","params":{"textDocument":{"uri":"file:///sketch.ino"},"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}}}}`
	input := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(request), request)
	output := &bytes.Buffer{}

	server := NewIDELSPServer(nil, strings.NewReader(input), output, nil)
	server.Run()

	require.Contains(t, output.String(), `{"jsonrpc":"2.0","id":1,"result":null}`)
}
