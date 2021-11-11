module github.com/arduino/arduino-language-server

go 1.12

replace go.bug.st/lsp => ../go-lsp

require (
	github.com/arduino/arduino-cli v0.0.0-20211111113528-bf4a7844a79b
	github.com/arduino/go-paths-helper v1.6.1
	github.com/fatih/color v1.13.0
	github.com/pkg/errors v0.9.1
	github.com/stretchr/testify v1.7.0
	go.bug.st/json v1.15.6
	go.bug.st/lsp v0.0.0-20211109230950-26242be380a2
)
