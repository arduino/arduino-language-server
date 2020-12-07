module github.com/bcmi-labs/arduino-language-server

go 1.12

replace github.com/arduino/arduino-cli => ../arduino-cli

require (
	github.com/arduino/arduino-cli v0.0.0-20201201130510-05ce1509a4f1
	github.com/arduino/go-paths-helper v1.4.0
	github.com/pkg/errors v0.9.1
	github.com/sourcegraph/jsonrpc2 v0.0.0-20200429184054-15c2290dcb37
	github.com/stretchr/testify v1.6.1
)
