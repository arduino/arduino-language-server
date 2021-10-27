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
	client   string
	server   string
	clColor  *color.Color
	srvColor *color.Color
}

func NewJsonRPCLogger(client, server string, weAreClient bool) *JsonRPCLogger {
	color.NoColor = false
	clColor := color.New(color.FgHiRed)
	srvColor := color.New(color.FgHiGreen)
	if !weAreClient {
		clColor, srvColor = srvColor, clColor
	}
	return &JsonRPCLogger{
		client:   client + " --> " + server + " ",
		server:   client + " <-- " + server + " ",
		clColor:  clColor,
		srvColor: srvColor,
	}
}

func empty(s string) string {
	return "                                                "[:len(s)]
}

func (l *JsonRPCLogger) LogClientRequest(method string, params json.RawMessage) (PrefixLogger, int64) {
	id := atomic.AddInt64(&index, 1)
	prefix := fmt.Sprintf("REQ %s %v: ", method, id)
	dec := ""
	log.Print(l.clColor.Sprintf(l.client+prefix+"%s", dec))
	return NewPrefixLogger(l.clColor, empty(l.client)+prefix), id
}

func (l *JsonRPCLogger) LogClientResponse(id int64, method string, params json.RawMessage, err *jsonrpc.ResponseError) {
	dec := ""
	if err != nil {
		dec += fmt.Sprintf("ERROR %v", err.AsError())
	}
	log.Print(l.clColor.Sprintf(l.client+"RESP %s %v: %s", method, id, dec))
}

func (l *JsonRPCLogger) LogClientNotification(method string, params json.RawMessage) PrefixLogger {
	prefix := fmt.Sprintf("NOTIF %s: ", method)
	dec := ""
	log.Print(l.clColor.Sprintf(l.client+prefix+"%s", dec))
	return NewPrefixLogger(l.clColor, empty(l.client)+prefix)
}

func (l *JsonRPCLogger) LogServerRequest(method string, params json.RawMessage) (PrefixLogger, int64) {
	id := atomic.AddInt64(&index, 1)

	prefix := fmt.Sprintf("REQ %s %v: ", method, id)
	dec := ""
	log.Print(l.srvColor.Sprintf(l.server+prefix+"%s", dec))
	return NewPrefixLogger(l.srvColor, empty(l.server)+prefix), id
}

func (l *JsonRPCLogger) LogServerResponse(id int64, method string, params json.RawMessage, err *jsonrpc.ResponseError) {
	dec := ""
	if err != nil {
		dec += fmt.Sprintf("ERROR %v", err.AsError())
	}
	log.Print(l.srvColor.Sprintf(l.server+"RESP %s %v: %s", method, id, dec))
}

func (l *JsonRPCLogger) LogServerNotification(method string, params json.RawMessage) PrefixLogger {
	prefix := fmt.Sprintf("NOTIF %s: ", method)
	dec := ""
	log.Print(l.srvColor.Sprintf(l.server+prefix+"%s", dec))
	return NewPrefixLogger(l.srvColor, empty(l.server)+prefix)
}

func decodeLspRequest(method string, req json.RawMessage) string {
	switch method {
	case "$/progress":
		// var begin lsp.WorkDoneProgressBegin
		// if json.Unmarshal(*v.Value, &begin) == nil {
		// 	return fmt.Sprintf("TOKEN=%s BEGIN %v %v", v.Token, begin.Title, begin.Message)
		// }
		// var report lsp.WorkDoneProgressReport
		// if json.Unmarshal(*v.Value, &report) == nil {
		// 	return fmt.Sprintf("TOKEN=%s REPORT %v %v%%", v.Token, report.Message, fmtFloat(report.Percentage))
		// }
		// var end lsp.WorkDoneProgressEnd
		// if json.Unmarshal(*v.Value, &end) == nil {
		// 	return fmt.Sprintf("TOKEN=%s END %v", v.Token, fmtString(end.Message))
		// }
		// return "UNKNOWN?"
		return "SOME PROGRESS..."
	default:
		return ""
	}
}
