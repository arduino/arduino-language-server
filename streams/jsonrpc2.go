package streams

import (
	"log"
	"runtime/debug"

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
		if req != nil {
			log.Print(c.Sprintf(prefix+" ANSWER %s %v (%v)", req.Method, req.ID, resp.ID))
		} else {
			log.Print(c.Sprintf(prefix+" ANSWER UNBOUND (%v)", resp.ID))
		}
	} else if req != nil {
		if !req.Notif {
			log.Print(c.Sprintf(prefix+" REQUEST %s %v", req.Method, req.ID))
		} else {
			log.Print(c.Sprintf(prefix+" NOTIFICATION %s", req.Method))
		}
	} else {
		log.Print(green.Sprintf(prefix + " NULL MESSAGE"))
		log.Print(string(debug.Stack()))
	}
}
