package handler

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/bcmi-labs/arduino-language-server/lsp"
)

func TestUriToPath(t *testing.T) {
	var path string
	if runtime.GOOS == "windows" {
		path = uriToPath(lsp.DocumentURI("file:///C:/Users/test/Sketch.ino"))
		if path != "C:\\Users\\test\\Sketch.ino" {
			t.Error(path)
		}
		path = uriToPath(lsp.DocumentURI("file:///c%3A/Users/test/Sketch.ino"))
		if path != "C:\\Users\\test\\Sketch.ino" {
			t.Error(path)
		}
	} else {
		path = uriToPath(lsp.DocumentURI("file:///Users/test/Sketch.ino"))
		if path != "/Users/test/Sketch.ino" {
			t.Error(path)
		}
	}
	path = uriToPath(lsp.DocumentURI("file:///%25F0%259F%2598%259B"))
	if path != string(filepath.Separator)+"\U0001F61B" {
		t.Error(path)
	}
}

func TestPathToUri(t *testing.T) {
	var uri lsp.DocumentURI
	if runtime.GOOS == "windows" {
		uri = pathToURI("C:\\Users\\test\\Sketch.ino")
		if uri != "file:///C:/Users/test/Sketch.ino" {
			t.Error(uri)
		}
	} else {
		uri = pathToURI("/Users/test/Sketch.ino")
		if uri != "file:///Users/test/Sketch.ino" {
			t.Error(uri)
		}
	}
	uri = pathToURI("\U0001F61B")
	if uri != "file:///%25F0%259F%2598%259B" {
		t.Error(uri)
	}
}
