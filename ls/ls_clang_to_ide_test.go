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
	"testing"

	"github.com/arduino/go-paths-helper"
	"github.com/stretchr/testify/require"
	"go.bug.st/lsp"
	"go.bug.st/lsp/jsonrpc"
)

func TestClang2IdeDiagnosticsClearedHaveNoVersion(t *testing.T) {
	tmp := paths.New(t.TempDir())
	ideIno := lsp.NewDocumentURIFromPath(tmp.Join("Sketch", "Sketch.ino"))
	buildSketchRoot := tmp.Join("build", "sketch")
	buildSketchCpp := buildSketchRoot.Join("Sketch.ino.cpp")
	// The file must exist: on Windows the URI round trip lowercases the
	// drive letter, so the paths only compare equal as the same file.
	require.NoError(t, buildSketchRoot.MkdirAll())
	require.NoError(t, buildSketchCpp.WriteFile(nil))
	ls := &INOLanguageServer{
		buildSketchRoot: buildSketchRoot,
		buildSketchCpp:  buildSketchCpp,
		trackedIdeDocs: map[string]lsp.TextDocumentItem{
			ideIno.AsPath().String(): {URI: ideIno, Version: 2},
		},
	}

	// clangd versions Sketch.ino.cpp itself, and sketch rebuilds bump that
	// version without any edit from the IDE, so it runs ahead of the .ino's.
	ideParams, err := ls.clang2IdeDiagnostics(&jsonrpc.NullFunctionLogger{}, &lsp.PublishDiagnosticsParams{
		URI:         lsp.NewDocumentURIFromPath(buildSketchCpp),
		Version:     4,
		Diagnostics: []lsp.Diagnostic{},
	})
	require.NoError(t, err)
	require.Contains(t, ideParams, ideIno)
	require.Empty(t, ideParams[ideIno].Diagnostics)
	// The .cpp's version means nothing to the IDE, which may drop a
	// notification whose version is not its own, leaving stale
	// diagnostics on screen. So it must not be forwarded.
	require.Zero(t, ideParams[ideIno].Version)
}
