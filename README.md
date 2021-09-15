<img src="https://content.arduino.cc/website/Arduino_logo_teal.svg" height="100" align="right" />

# Arduino Language Server

The **Arduino Language Server** is the tool that powers the autocompletion of the new [Arduino IDE 2][arduino-ide-repo]. It implements the standard [Language Server Protocol](https://microsoft.github.io/language-server-protocol/) so it can be used with other IDEs as well.

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
- Open your PR against the master branch.
- Maintain clean commit history and use meaningful commit messages. PRs with messy commit history are difficult to review and require a lot of work to be merged.
- Your PR must pass all CI tests before we will merge it. If you're seeing an error and don't think it's your fault, it may not be! The reviewer will help you if there are test failures that seem not related to the change you are making.

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

To run the Arduino Language Server you need:

- [Arduino CLI](https://github.com/arduino/arduino-cli)

After building, call:

```
./arduino-language-server -cli-config <path-to-cli-config>
```
For example:
```
./arduino-language-server -cli-config $HOME/.arduino15/arduino-cli.yaml
```
Note: If you do not have an Arduino CLI config file, you can create one by running:
```
arduino-cli config init
```

## Donations

This open source code was written by the Arduino team and is maintained on a daily basis with the help of the community. We invest a considerable amount of time in development, testing and optimization. Please consider [donating](https://www.arduino.cc/en/donate/) or [sponsoring](https://github.com/sponsors/arduino) to support our work, as well as [buying original Arduino boards](https://store.arduino.cc/) which is the best way to make sure our effort can continue in the long term.

## License

The code contained in this repository is licensed under the terms of the Apache 2.0 license. If you have questions about licensing please contact us at [license@arduino.cc](mailto:license@arduino.cc).


[arduino-ide-repo]: https://github.com/arduino/arduino-ide
[go-install]: https://golang.org/doc/install
