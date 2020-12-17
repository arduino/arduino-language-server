package lsp

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestUriToPath(t *testing.T) {
	var path string
	if runtime.GOOS == "windows" {
		path = DocumentURI("file:///C:/Users/test/Sketch.ino").Unbox()
		if path != "C:\\Users\\test\\Sketch.ino" {
			t.Error(path)
		}
		path = DocumentURI("file:///c%3A/Users/test/Sketch.ino").Unbox()
		if path != "C:\\Users\\test\\Sketch.ino" {
			t.Error(path)
		}
	} else {
		path = DocumentURI("file:///Users/test/Sketch.ino").Unbox()
		if path != "/Users/test/Sketch.ino" {
			t.Error(path)
		}
	}
	path = DocumentURI("file:///%25F0%259F%2598%259B").Unbox()
	if path != string(filepath.Separator)+"\U0001F61B" {
		t.Error(path)
	}
}

func TestPathToUri(t *testing.T) {
	var uri DocumentURI
	if runtime.GOOS == "windows" {
		uri = NewDocumentURI("C:\\Users\\test\\Sketch.ino")
		if uri != "file:///C:/Users/test/Sketch.ino" {
			t.Error(uri)
		}
	} else {
		uri = NewDocumentURI("/Users/test/Sketch.ino")
		if uri != "file:///Users/test/Sketch.ino" {
			t.Error(uri)
		}
	}
	uri = NewDocumentURI("\U0001F61B")
	if uri != "file:///%25F0%259F%2598%259B" {
		t.Error(uri)
	}
}
