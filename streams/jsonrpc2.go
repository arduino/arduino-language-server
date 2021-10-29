package streams

import (
	"fmt"
	"log"
	"sync/atomic"

	"github.com/fatih/color"
	"go.bug.st/json"
	"go.bug.st/lsp/jsonrpc"
)

var index int64

type PrefixLogger func(format string, a ...interface{})

func NewPrefixLogger(col *color.Color, prefix string) PrefixLogger {
	return func(format string, a ...interface{}) {
		log.Print(col.Sprintf(prefix+format, a...))
	}
}

type JsonRPCLogger struct {
	client string
	server string
}

var clColor = color.New(color.FgHiRed)
var srvColor = color.New(color.FgHiGreen)

func NewJsonRPCLogger(client, server string) *JsonRPCLogger {
	color.NoColor = false
	return &JsonRPCLogger{
		client: client + " --> " + server + " ",
		server: client + " <-- " + server + " ",
	}
}

func empty(s string) string {
	return "                                                "[:len(s)]
}

func (l *JsonRPCLogger) LogClientRequest(method string, params json.RawMessage) (PrefixLogger, int64) {
	id := atomic.AddInt64(&index, 1)
	prefix := fmt.Sprintf("REQ %s %v: ", method, id)
	dec := ""
	log.Print(clColor.Sprintf(l.client+prefix+"%s", dec))
	return NewPrefixLogger(clColor, empty(l.client)+prefix), id
}

func (l *JsonRPCLogger) LogClientResponse(id int64, method string, params json.RawMessage, err *jsonrpc.ResponseError) {
	dec := ""
	if err != nil {
		dec += fmt.Sprintf("ERROR %v", err.AsError())
	}
	log.Print(clColor.Sprintf(l.client+"RESP %s %v: %s", method, id, dec))
}

func (l *JsonRPCLogger) LogClientNotification(method string, params json.RawMessage) PrefixLogger {
	prefix := fmt.Sprintf("NOTIF %s: ", method)
	dec := ""
	log.Print(clColor.Sprintf(l.client+prefix+"%s", dec))
	return NewPrefixLogger(clColor, empty(l.client)+prefix)
}

func (l *JsonRPCLogger) LogServerRequest(method string, params json.RawMessage) (PrefixLogger, int64) {
	id := atomic.AddInt64(&index, 1)

	prefix := fmt.Sprintf("REQ %s %v: ", method, id)
	dec := ""
	log.Print(srvColor.Sprintf(l.server+prefix+"%s", dec))
	return NewPrefixLogger(srvColor, empty(l.server)+prefix), id
}

func (l *JsonRPCLogger) LogServerResponse(id int64, method string, params json.RawMessage, err *jsonrpc.ResponseError) {
	dec := ""
	if err != nil {
		dec += fmt.Sprintf("ERROR %v", err.AsError())
	}
	log.Print(srvColor.Sprintf(l.server+"RESP %s %v: %s", method, id, dec))
}

func (l *JsonRPCLogger) LogServerNotification(method string, params json.RawMessage) PrefixLogger {
	prefix := fmt.Sprintf("NOTIF %s: ", method)
	dec := ""
	log.Print(srvColor.Sprintf(l.server+prefix+"%s", dec))
	return NewPrefixLogger(srvColor, empty(l.server)+prefix)
}
