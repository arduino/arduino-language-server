package ls

import (
	"regexp"
	"strings"

	"github.com/arduino/arduino-language-server/streams"
	"github.com/pkg/errors"
	"go.bug.st/lsp"
	"go.bug.st/lsp/jsonrpc"
)

func (ls *INOLanguageServer) handleError(logger jsonrpc.FunctionLogger, err error) error {
	errorStr := err.Error()
	var message string
	if strings.Contains(errorStr, "#error") {
		exp, regexpErr := regexp.Compile("#error \"(.*)\"")
		if regexpErr != nil {
			panic(regexpErr)
		}
		submatch := exp.FindStringSubmatch(errorStr)
		message = submatch[1]
	} else if strings.Contains(errorStr, "platform not installed") || strings.Contains(errorStr, "no FQBN provided") {
		if ls.config.Fqbn != "" {
			message = "Editor support may be inaccurate because the core for the board `" + ls.config.Fqbn + "` is not installed."
			message += " Use the Boards Manager to install it."
		} else {
			// This case happens most often when the app is started for the first time and no
			// board is selected yet. Don't bother the user with an error then.
			return err
		}
	} else if strings.Contains(errorStr, "No such file or directory") {
		exp, regexpErr := regexp.Compile(`([\w\.\-]+): No such file or directory`)
		if regexpErr != nil {
			panic(regexpErr)
		}
		submatch := exp.FindStringSubmatch(errorStr)
		message = "Editor support may be inaccurate because the header `" + submatch[1] + "` was not found."
		message += " If it is part of a library, use the Library Manager to install it."
	} else {
		message = "Could not start editor support.\n" + errorStr
	}
	go func() {
		defer streams.CatchAndLogPanic()
		ls.showMessage(logger, lsp.MessageTypeError, message)
	}()
	return errors.New(message)
}

func (ls *INOLanguageServer) showMessage(logger jsonrpc.FunctionLogger, msgType lsp.MessageType, message string) {
	params := lsp.ShowMessageParams{
		Type:    msgType,
		Message: message,
	}
	if err := ls.IDE.conn.WindowShowMessage(&params); err != nil {
		logger.Logf("error sending showMessage notification: %s", err)
	}
}
