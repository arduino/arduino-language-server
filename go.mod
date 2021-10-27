module github.com/arduino/arduino-language-server

go 1.12

replace go.bug.st/lsp => ../lsp

replace go.bug.st/json => ../go-json

require (
	github.com/arduino/arduino-cli v0.0.0-20201215104024-6a177ebf56f2
	github.com/arduino/go-paths-helper v1.6.1
	github.com/fatih/color v1.7.0
	github.com/pkg/errors v0.9.1
	github.com/sourcegraph/jsonrpc2 v0.0.0-20200429184054-15c2290dcb37
	github.com/stretchr/testify v1.6.1
	go.bug.st/json v1.0.0
	go.bug.st/lsp v0.0.0-00010101000000-000000000000
)
