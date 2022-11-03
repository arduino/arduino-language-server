// This file is part of arduino-language-server.
//
// Copyright 2022 ARDUINO SA (http://www.arduino.cc/)
//
// This software is released under the GNU Affero General Public License version 3,
// which covers the main part of arduino-language-server.
// The terms of this license can be found at:
// https://www.gnu.org/licenses/agpl-3.0.html
//
// You can be released from the requirements of the above licenses by purchasing
// a commercial license. Buying such a license is mandatory if you want to
// modify or otherwise use the software for commercial activities involving the
// Arduino software without disclosing the source code of your own applications.
// To purchase a commercial license, send an email to license@arduino.cc.

package ls

import (
	"fmt"
	"log"
	"time"

	"github.com/fatih/color"
	"go.bug.st/json"
	"go.bug.st/lsp/jsonrpc"
)

// Logger is a lsp logger
type Logger struct {
	IncomingPrefix, OutgoingPrefix string
	HiColor, LoColor               func(format string, a ...interface{}) string
	ErrorColor                     func(format string, a ...interface{}) string
}

func init() {
	log.SetFlags(log.Lmicroseconds)
}

// LogOutgoingRequest prints an outgoing request into the log
func (l *Logger) LogOutgoingRequest(id string, method string, params json.RawMessage) {
	log.Print(l.HiColor("%s REQU %s %s", l.OutgoingPrefix, method, id))
}

// LogOutgoingCancelRequest prints an outgoing cancel request into the log
func (l *Logger) LogOutgoingCancelRequest(id string) {
	log.Print(l.LoColor("%s CANCEL %s", l.OutgoingPrefix, id))
}

// LogIncomingResponse prints an incoming response into the log if there is no error
func (l *Logger) LogIncomingResponse(id string, method string, resp json.RawMessage, respErr *jsonrpc.ResponseError) {
	e := ""
	if respErr != nil {
		e = l.ErrorColor(" ERROR: %s", respErr.AsError())
	}
	log.Print(l.LoColor("%s RESP %s %s%s", l.IncomingPrefix, method, id, e))
}

// LogOutgoingNotification prints an outgoing notification into the log
func (l *Logger) LogOutgoingNotification(method string, params json.RawMessage) {
	log.Print(l.HiColor("%s NOTIF %s", l.OutgoingPrefix, method))
}

// LogIncomingRequest prints an incoming request into the log
func (l *Logger) LogIncomingRequest(id string, method string, params json.RawMessage) jsonrpc.FunctionLogger {
	spaces := "                                               "
	log.Print(l.HiColor(fmt.Sprintf("%s REQU %s %s", l.IncomingPrefix, method, id)))
	return &FunctionLogger{
		colorFunc: l.HiColor,
		prefix:    fmt.Sprintf("%s      %s %s", spaces[:len(l.IncomingPrefix)], method, id),
	}
}

// LogIncomingCancelRequest prints an incoming cancel request into the log
func (l *Logger) LogIncomingCancelRequest(id string) {
	log.Print(l.LoColor("%s CANCEL %s", l.IncomingPrefix, id))
}

// LogOutgoingResponse prints an outgoing response into the log if there is no error
func (l *Logger) LogOutgoingResponse(id string, method string, resp json.RawMessage, respErr *jsonrpc.ResponseError) {
	e := ""
	if respErr != nil {
		e = l.ErrorColor(" ERROR: %s", respErr.AsError())
	}
	log.Print(l.LoColor("%s RESP %s %s%s", l.OutgoingPrefix, method, id, e))
}

// LogIncomingNotification prints an incoming notification into the log
func (l *Logger) LogIncomingNotification(method string, params json.RawMessage) jsonrpc.FunctionLogger {
	spaces := "                                               "
	log.Print(l.HiColor(fmt.Sprintf("%s NOTIF %s", l.IncomingPrefix, method)))
	return &FunctionLogger{
		colorFunc: l.HiColor,
		prefix:    fmt.Sprintf("%s       %s", spaces[:len(l.IncomingPrefix)], method),
	}
}

// LogIncomingDataDelay prints the delay of incoming data into the log
func (l *Logger) LogIncomingDataDelay(delay time.Duration) {
	log.Printf("IN Elapsed: %v", delay)
}

// LogOutgoingDataDelay prints the delay of outgoing data into the log
func (l *Logger) LogOutgoingDataDelay(delay time.Duration) {
	log.Printf("OUT Elapsed: %v", delay)
}

// FunctionLogger is a lsp function logger
type FunctionLogger struct {
	colorFunc func(format string, a ...interface{}) string
	prefix    string
}

// NewLSPFunctionLogger creates a new function logger
func NewLSPFunctionLogger(colofFunction func(format string, a ...interface{}) string, prefix string) *FunctionLogger {
	color.NoColor = false
	return &FunctionLogger{
		colorFunc: colofFunction,
		prefix:    prefix,
	}
}

// Logf logs the given message
func (l *FunctionLogger) Logf(format string, a ...interface{}) {
	log.Print(l.colorFunc(l.prefix+": "+format, a...))
}
