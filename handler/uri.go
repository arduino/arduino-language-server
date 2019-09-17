package handler

import (
	"net/url"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/pkg/errors"
	lsp "github.com/sourcegraph/go-lsp"
)

var expDriveId = regexp.MustCompile("[a-zA-Z]:")

func uriToPath(uri lsp.DocumentURI) string {
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
		if runtime.GOOS == "windows" && expDriveId.MatchString(decoded) {
			path += strings.ToUpper(decoded)
		} else if len(decoded) > 0 {
			path += string(filepath.Separator) + decoded
		}
	}
	return path
}

func pathToURI(path string) lsp.DocumentURI {
	urlObj, err := url.Parse("file://")
	if err != nil {
		panic(err)
	}
	segments := strings.Split(path, string(filepath.Separator))
	for _, segment := range segments {
		urlObj.Path += "/" + url.PathEscape(segment)
	}
	return lsp.DocumentURI(urlObj.String())
}

func unknownURI(uri lsp.DocumentURI) error {
	return errors.New("Document is not available: " + string(uri))
}
