package lsp

import (
	"net/url"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/arduino/go-paths-helper"
)

var expDriveID = regexp.MustCompile("[a-zA-Z]:")

// AsPath convert the DocumentURI to a paths.Path
func (uri DocumentURI) AsPath() *paths.Path {
	return paths.New(uri.Unbox())
}

// Unbox convert the DocumentURI to a file path string
func (uri DocumentURI) Unbox() string {
	urlObj, err := url.Parse(string(uri))
	if err != nil {
		return string(uri)
	}
	path := ""
	segments := strings.Split(urlObj.Path, "/")
	for _, segment := range segments {
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			decoded = segment
		}
		if runtime.GOOS == "windows" && expDriveID.MatchString(decoded) {
			path += strings.ToUpper(decoded)
		} else if len(decoded) > 0 {
			path += string(filepath.Separator) + decoded
		}
	}
	return path
}

func (uri DocumentURI) String() string {
	return string(uri)
}

// NewDocumentURIFromPath create a DocumentURI from the given Path object
func NewDocumentURIFromPath(path *paths.Path) DocumentURI {
	return NewDocumentURI(path.String())
}

// NewDocumentURI create a DocumentURI from the given string path
func NewDocumentURI(path string) DocumentURI {
	urlObj, err := url.Parse("file://")
	if err != nil {
		panic(err)
	}
	segments := strings.Split(path, string(filepath.Separator))
	for _, segment := range segments {
		if len(segment) > 0 {
			urlObj.Path += "/" + url.PathEscape(segment)
		}
	}
	return DocumentURI(urlObj.String())
}
