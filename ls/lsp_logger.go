package ls

import (
	"fmt"
	"log"
	"time"

	"github.com/fatih/color"
	"go.bug.st/json"
	"go.bug.st/lsp/jsonrpc"
)

type LSPLogger struct {
	IncomingPrefix, OutgoingPrefix string
	HiColor, LoColor               func(format string, a ...interface{}) string
	ErrorColor                     func(format string, a ...interface{}) string
}

func init() {
	log.SetFlags(log.Lmicroseconds)
}

func (l *LSPLogger) LogOutgoingRequest(id string, method string, params json.RawMessage) {
	log.Print(l.HiColor("%s REQU %s %s", l.OutgoingPrefix, method, id))
}
func (l *LSPLogger) LogOutgoingCancelRequest(id string) {
	log.Print(l.LoColor("%s CANCEL %s", l.OutgoingPrefix, id))
}
func (l *LSPLogger) LogIncomingResponse(id string, method string, resp json.RawMessage, respErr *jsonrpc.ResponseError) {
	e := ""
	if respErr != nil {
		e = l.ErrorColor(" ERROR: %s", respErr.AsError())
	}
	log.Print(l.LoColor("%s RESP %s %s%s", l.IncomingPrefix, method, id, e))
}
func (l *LSPLogger) LogOutgoingNotification(method string, params json.RawMessage) {
	log.Print(l.HiColor("%s NOTIF %s", l.OutgoingPrefix, method))
}

func (l *LSPLogger) LogIncomingRequest(id string, method string, params json.RawMessage) jsonrpc.FunctionLogger {
	spaces := "                                               "
	log.Print(l.HiColor(fmt.Sprintf("%s REQU %s %s", l.IncomingPrefix, method, id)))
	return &LSPFunctionLogger{
		colorFunc: l.HiColor,
		prefix:    fmt.Sprintf("%s      %s %s", spaces[:len(l.IncomingPrefix)], method, id),
	}
}
func (l *LSPLogger) LogIncomingCancelRequest(id string) {
	log.Print(l.LoColor("%s CANCEL %s", l.IncomingPrefix, id))
}
func (l *LSPLogger) LogOutgoingResponse(id string, method string, resp json.RawMessage, respErr *jsonrpc.ResponseError) {
	e := ""
	if respErr != nil {
		e = l.ErrorColor(" ERROR: %s", respErr.AsError())
	}
	log.Print(l.LoColor("%s RESP %s %s%s", l.OutgoingPrefix, method, id, e))
}
func (l *LSPLogger) LogIncomingNotification(method string, params json.RawMessage) jsonrpc.FunctionLogger {
	spaces := "                                               "
	log.Print(l.HiColor(fmt.Sprintf("%s NOTIF %s", l.IncomingPrefix, method)))
	return &LSPFunctionLogger{
		colorFunc: l.HiColor,
		prefix:    fmt.Sprintf("%s       %s", spaces[:len(l.IncomingPrefix)], method),
	}
}
func (l *LSPLogger) LogIncomingDataDelay(delay time.Duration) {
	log.Printf("IN Elapsed: %v", delay)
}
func (l *LSPLogger) LogOutgoingDataDelay(delay time.Duration) {
	log.Printf("OUT Elapsed: %v", delay)
}

type LSPFunctionLogger struct {
	colorFunc func(format string, a ...interface{}) string
	prefix    string
}

func NewLSPFunctionLogger(colofFunction func(format string, a ...interface{}) string, prefix string) *LSPFunctionLogger {
	color.NoColor = false
	return &LSPFunctionLogger{
		colorFunc: colofFunction,
		prefix:    prefix,
	}
}

func (l *LSPFunctionLogger) Logf(format string, a ...interface{}) {
	log.Print(l.colorFunc(l.prefix+": "+format, a...))
}
