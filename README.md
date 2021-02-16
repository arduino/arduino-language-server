# Arduino Language Server

**Arduino Language Server** is the the tool that powers the autocompletion of the new [Arduino IDE 2][arduino-ide-repo].

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

## How to contribute

Contributions are welcome! Here are all the way you can contribute to the project.

### Issue Reports

High quality bug reports and feature requests are valuable contributions to the project.

Before reporting an issue search existing pull requests and issues to see if it was already reported. If you have additional information to provide about an existing issue, please comment there. You can use the Reactions feature if you only want to express support.

Qualities of an excellent report

- The issue title should be descriptive. Vague titles make it difficult to understand the purpose of the issue, which might cause your issue to be overlooked.
- Provide a full set of steps necessary to reproduce the issue. Demonstration code or commands should be complete and simplified to the minimum necessary to reproduce the issue.
- Be responsive. We may need you to provide additional information in order to investigate and resolve the issue.
- If you find a solution to your problem, please comment on your issue report with an explanation of how you were able to fix it and close the issue.

### Pull Requests

To propose improvements or fix a bug, feel free to submit a PR.

### Legal requirements

Before we can accept your contributions you have to sign the Contributor License Agreement
Pull request checklist

### Pull request checklist

In order to ease code reviews and have your contributions merged faster, here is a list of items you can check before submitting a PR:

- Create small PRs that are narrowly focused on addressing a single concern.
- Write tests for the code you wrote.
- Open your PR against the master branch.
- Maintain clean commit history and use meaningful commit messages. PRs with messy commit history are difficult to review and require a lot of work to be merged.
- Your PR must pass all CI tests before we will merge it. If you're seeing an error and don't think it's your fault, it may not be! The reviewer will help you if there are test failures that seem not related to the change you are making.

[arduino-ide-repo]: https://github.com/arduino/arduino-ide
[go-install]: https://golang.org/doc/install
