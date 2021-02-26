package streams

import (
	"encoding/json"
	"fmt"
	"log"
	"runtime/debug"

	"github.com/arduino/arduino-language-server/lsp"
	"github.com/fatih/color"
	"github.com/sourcegraph/jsonrpc2"
)

var green = color.New(color.FgHiGreen)
var red = color.New(color.FgHiRed)

// JSONRPCConnLogOnRecv perform logging of the given req and resp
func JSONRPCConnLogOnRecv(prefix string) func(req *jsonrpc2.Request, resp *jsonrpc2.Response) {
	return func(req *jsonrpc2.Request, resp *jsonrpc2.Response) {
		jsonrpcLog(prefix, req, resp, false)
	}
}

// JSONRPCConnLogOnSend perform logging of the given req and resp
func JSONRPCConnLogOnSend(prefix string) func(req *jsonrpc2.Request, resp *jsonrpc2.Response) {
	return func(req *jsonrpc2.Request, resp *jsonrpc2.Response) {
		jsonrpcLog(prefix, req, resp, true)
	}
}

func jsonrpcLog(prefix string, req *jsonrpc2.Request, resp *jsonrpc2.Response, sending bool) {
	color.NoColor = false
	var c *color.Color
	if sending {
		c = red
	} else {
		c = green
	}
	if resp != nil {
		dec := jsonrpcLogDecodeResp(resp)
		if req != nil {
			log.Print(c.Sprintf(prefix+" ANSWER %s %v (%v): %s", req.Method, req.ID, resp.ID, dec))
		} else {
			log.Print(c.Sprintf(prefix+" ANSWER UNBOUND (%v): %s", resp.ID, dec))
		}
	} else if req != nil {
		dec := jsonrpcLogDecodeReq(req)
		if !req.Notif {
			log.Print(c.Sprintf(prefix+" REQUEST %s %v: %s", req.Method, req.ID, dec))
		} else {
			log.Print(c.Sprintf(prefix+" NOTIFICATION %s: %s", req.Method, dec))
		}
	} else {
		log.Print(green.Sprintf(prefix + " NULL MESSAGE"))
		log.Print(string(debug.Stack()))
	}
}

func jsonrpcLogDecodeReq(req *jsonrpc2.Request) string {
	fmtString := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	fmtFloat := func(s *float64) float64 {
		if s == nil {
			return 0
		}
		return *s
	}
	switch req.Method {
	case "$/progress":
		var v lsp.ProgressParams
		if err := json.Unmarshal(*req.Params, &v); err != nil {
			return err.Error()
		}
		var begin lsp.WorkDoneProgressBegin
		if json.Unmarshal(*v.Value, &begin) == nil {
			return fmt.Sprintf("TOKEN=%s BEGIN %v %v", v.Token, begin.Title, fmtString(begin.Message))
		}
		var report lsp.WorkDoneProgressReport
		if json.Unmarshal(*v.Value, &report) == nil {
			return fmt.Sprintf("TOKEN=%s REPORT %v %v%%", v.Token, fmtString(report.Message), fmtFloat(report.Percentage))
		}
		var end lsp.WorkDoneProgressEnd
		if json.Unmarshal(*v.Value, &end) == nil {
			return fmt.Sprintf("TOKEN=%s END %v", v.Token, fmtString(end.Message))
		}
		return "UNKNOWN?"
	default:
		return ""
	}
}

func jsonrpcLogDecodeResp(resp *jsonrpc2.Response) string {
	return ""
}
