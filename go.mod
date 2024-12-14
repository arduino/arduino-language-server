module github.com/arduino/arduino-language-server

go 1.22

toolchain go1.22.8

require (
	github.com/arduino/arduino-cli v1.0.3
	github.com/arduino/go-paths-helper v1.12.1
	github.com/fatih/color v1.17.0
	github.com/mattn/go-isatty v0.0.20
	github.com/pkg/errors v0.9.1
	github.com/stretchr/testify v1.9.0
	go.bug.st/json v1.15.6
	go.bug.st/lsp v0.1.2
	google.golang.org/grpc v1.65.0
)

require (
	github.com/arduino/go-properties-orderedmap v1.8.1 // indirect
	github.com/arduino/pluggable-discovery-protocol-handler/v2 v2.2.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	go.bug.st/relaxed-semver v0.12.0 // indirect
	golang.org/x/net v0.25.0 // indirect
	golang.org/x/sys v0.22.0 // indirect
	golang.org/x/text v0.16.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240528184218-531527333157 // indirect
	google.golang.org/protobuf v1.34.2 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace go.bug.st/lsp => github.com/speelbarrow/go-lsp v0.1.3-0.20241103164431-cf1c00fb5806
