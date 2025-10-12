<img src="https://content.arduino.cc/website/Arduino_logo_teal.svg" height="100" align="right" />

# Arduino Language Server

[![Check Taskfiles status](https://github.com/arduino/arduino-language-server/actions/workflows/check-taskfiles.yml/badge.svg)](https://github.com/arduino/arduino-language-server/actions/workflows/check-taskfiles.yml)
[![Check Go status](https://github.com/arduino/arduino-language-server/actions/workflows/check-go-task.yml/badge.svg)](https://github.com/arduino/arduino-language-server/actions/workflows/check-go-task.yml)
[![Check Markdown status](https://github.com/arduino/arduino-language-server/actions/workflows/check-markdown-task.yml/badge.svg)](https://github.com/arduino/arduino-language-server/actions/workflows/check-markdown-task.yml)
[![Check License status](https://github.com/arduino/arduino-language-server/actions/workflows/check-license.yml/badge.svg)](https://github.com/arduino/arduino-language-server/actions/workflows/check-license.yml)
[![Check Go Dependencies status](https://github.com/arduino/arduino-language-server/actions/workflows/check-go-dependencies-task.yml/badge.svg)](https://github.com/arduino/arduino-language-server/actions/workflows/check-go-dependencies-task.yml)

The **Arduino Language Server** is the tool that powers the autocompletion of the new [Arduino IDE 2][arduino-ide-repo]. It implements the standard [Language Server Protocol](https://microsoft.github.io/language-server-protocol/) so it can be used with other IDEs as well.

## Use Outside of Arduino IDE

The Arduino Language Server can be used with any editor that supports the Language Server Protocol. Depending on your IDE, you may need to manually manage its installation. You can do so using `go install`:

```bash
go install github.com/arduino/arduino-language-server@${VERSION}
```

> **NOTE** The `main` branch is **not** considered stable! It is *highly* recommended that you pin your installation (regardless of method) to a stable release. The latest release is
[![Latest Release](https://img.shields.io/github/v/release/arduino/arduino-language-server)](https://github.com/arduino/arduino-language-server/releases/latest).

## Bugs & Issues

High quality bug reports and feature requests are valuable contributions to the project.

Before reporting an issue search existing pull requests and issues to see if it was already reported. If you have additional information to provide about an existing issue, please comment there. You can use the Reactions feature if you only want to express support.

Qualities of an excellent report:

- The issue title should be descriptive. Vague titles make it difficult to understand the purpose of the issue, which might cause your issue to be overlooked.
- Provide a full set of steps necessary to reproduce the issue. Demonstration code or commands should be complete and simplified to the minimum necessary to reproduce the issue.
- Be responsive. We may need you to provide additional information in order to investigate and resolve the issue.
- If you find a solution to your problem, please comment on your issue report with an explanation of how you were able to fix it and close the issue.

### Security

If you think you found a vulnerability or other security-related bug in this project, please read our
[security policy](https://github.com/arduino/arduino-language-server/security/policy) and report the bug to our Security Team 🛡️
Thank you!

e-mail contact: security@arduino.cc

## How to contribute

Contributions are welcome! Here are all the ways you can contribute to the project.

### Pull Requests

To propose improvements or fix a bug, feel free to submit a PR.

### Pull request checklist

In order to ease code reviews and have your contributions merged faster, here is a list of items you can check before submitting a PR:

- Create small PRs that are narrowly focused on addressing a single concern.
- Write tests for the code you wrote.
- Open your PR against the `main` branch.
- Maintain clean commit history and use meaningful commit messages. PRs with messy commit history are difficult to review and require a lot of work to be merged.
- Your PR must pass all CI tests before we will merge it. If you're seeing an error and don't think it's your fault, it may not be! The reviewer will help you if there are test failures that seem not related to the change you are making.

### Support the project

This open source code was written by the Arduino team and is maintained on a daily basis with the help of the community. We invest a considerable amount of time in development, testing and optimization. Please consider [buying original Arduino boards](https://store.arduino.cc/) to support our work on the project.

## Build

To build the Arduino Language Server you need:

- [Go][go-install] version 1.12 or later

The project doesn't require `CGO` so it can be easily crosscompiled if necessary. To build for you machine just run:

```
go build
```

To run tests:

```
go test -v ./...
```

## Usage

The language server it's not intended for direct usage by humans via the command line terminal.
The purpose of this program is to provide C++/.ino language-related functionality to the IDEs so, in general, it's the IDE that talks to the language server via stdin/stdout using the slightly modified JSONRPC protocol defined in the LSP specification.

The prerequisites to run the Arduino Language Server are:

- [Arduino CLI](https://github.com/arduino/arduino-cli)
- [clangd](https://github.com/clangd/clangd/releases)

To start the language server the IDE may provide the path to Arduino CLI and clangd with the following flags in addition to the target board FQBN:

```
./arduino-language-server \
 -clangd /usr/local/bin/clangd \
 -cli /usr/local/bin/arduino-cli \
 -cli-config $HOME/.arduino15/arduino-cli.yaml \
 -fqbn arduino:mbed:nanorp2040connect
```

The -fqbn flag represents the board you're actually working on (different boards may implement different features/API, if you change board you need to restart the language server with another fqbn).
The support for the board must be installed with the `arduino-cli core install ...` command before starting the language server.

If you do not have an Arduino CLI config file, you can create one by running:

```
arduino-cli config init
```

## License

The code contained in this repository is licensed under the terms of the GNU Affero General Public License version 3 license. If you have questions about licensing please contact us at [license@arduino.cc](mailto:license@arduino.cc).

[arduino-ide-repo]: https://github.com/arduino/arduino-ide
[go-install]: https://golang.org/doc/install
