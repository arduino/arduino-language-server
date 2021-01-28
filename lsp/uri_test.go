package lsp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUriToPath(t *testing.T) {
	d, err := NewDocumentURIFromURL("file:///C:/Users/test/Sketch.ino")
	require.NoError(t, err)
	require.Equal(t, "C:/Users/test/Sketch.ino", d.unbox())

	d, err = NewDocumentURIFromURL("file:///c%3A/Users/test/Sketch.ino")
	require.NoError(t, err)
	require.Equal(t, "c:/Users/test/Sketch.ino", d.unbox())

	d, err = NewDocumentURIFromURL("file:///Users/test/Sketch.ino")
	require.NoError(t, err)
	require.Equal(t, "/Users/test/Sketch.ino", d.unbox())

	d, err = NewDocumentURIFromURL("file:///c%3A/Users/USERNA~1/AppData/Local/Temp/.arduinoProIDE-unsaved202108-10416-j28c17.lru6k/sketch_jan8a/sketch_jan8a.ino")
	require.NoError(t, err)
	require.Equal(t, "c:/Users/USERNA~1/AppData/Local/Temp/.arduinoProIDE-unsaved202108-10416-j28c17.lru6k/sketch_jan8a/sketch_jan8a.ino", d.unbox())

	d, err = NewDocumentURIFromURL("file:///%F0%9F%98%9B")
	require.NoError(t, err)
	require.Equal(t, "/\U0001F61B", d.unbox())
}

func TestPathToUri(t *testing.T) {
	toSlash = windowsToSlash // Emulate windows cases

	d := NewDocumentURI("C:\\Users\\test\\Sketch.ino")
	require.Equal(t, "file:///C:/Users/test/Sketch.ino", d.String())
	d = NewDocumentURI("/Users/test/Sketch.ino")
	require.Equal(t, "file:///Users/test/Sketch.ino", d.String())
	d = NewDocumentURI("\U0001F61B")
	require.Equal(t, "file:///%F0%9F%98%9B", d.String())
}

func TestJSONMarshalUnmarshal(t *testing.T) {
	toSlash = windowsToSlash // Emulate windows cases

	var d DocumentURI
	err := json.Unmarshal([]byte(`"file:///Users/test/Sketch.ino"`), &d)
	require.NoError(t, err)
	require.Equal(t, "/Users/test/Sketch.ino", d.unbox())

	err = json.Unmarshal([]byte(`"file:///%F0%9F%98%9B"`), &d)
	require.NoError(t, err)
	require.Equal(t, "/\U0001F61B", d.unbox())

	d = NewDocumentURI("C:\\Users\\test\\Sketch.ino")
	data, err := json.Marshal(d)
	require.NoError(t, err)
	require.Equal(t, `"file:///C:/Users/test/Sketch.ino"`, string(data))

	d = NewDocumentURI("/Users/test/Sketch.ino")
	data, err = json.Marshal(d)
	require.NoError(t, err)
	require.Equal(t, `"file:///Users/test/Sketch.ino"`, string(data))

	d = NewDocumentURI("/User nàmé/test/Sketch.ino")
	data, err = json.Marshal(d)
	require.NoError(t, err)
	require.Equal(t, `"file:///User%20n%C3%A0m%C3%A9/test/Sketch.ino"`, string(data))

	d = NewDocumentURI("\U0001F61B")
	data, err = json.Marshal(d)
	require.NoError(t, err)
	require.Equal(t, `"file:///%F0%9F%98%9B"`, string(data))
}

func TestNotInoFromSourceMapper(t *testing.T) {
	d, err := NewDocumentURIFromURL("file:///not-ino")
	require.NoError(t, err)
	fmt.Println(d.String())
	fmt.Println(d.unbox())
}

func windowsToSlash(path string) string {
	return strings.ReplaceAll(path, `\`, "/")
}

func windowsFromSlash(path string) string {
	return strings.ReplaceAll(path, "/", `\`)
}
