package handler

import (
	"context"
	"log"
	"sync"

	"github.com/sourcegraph/jsonrpc2"
)

// Synchronizer is used to block message processing while an edit or config change is applied.
type Synchronizer struct {
	// FileMux is a read/write mutex for file access. It is locked during the processing of
	// messages that modify target files for clangd.
	FileMux sync.RWMutex
	// DataMux is a mutex for document metadata access, i.e. source-target URI mappings and line mappings.
	DataMux sync.RWMutex
}

// AsyncHandler wraps a Handler such that each request is handled in its own goroutine.
type AsyncHandler struct {
	handler      jsonrpc2.Handler
	synchronizer *Synchronizer
}

// Handle handles a request or notification
func (ah AsyncHandler) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	needsWriteLock := req.Method == "textDocument/didOpen" || req.Method == "textDocument/didChange"
	if needsWriteLock {
		ah.synchronizer.FileMux.Lock()
		if enableLogging {
			log.Println("Message processing locked for", req.Method)
		}
		go ah.runWrite(ctx, conn, req)
	} else {
		ah.synchronizer.FileMux.RLock()
		go ah.runRead(ctx, conn, req)
	}
}

func (ah AsyncHandler) runRead(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	defer ah.synchronizer.FileMux.RUnlock()
	ah.handler.Handle(ctx, conn, req)
}

func (ah AsyncHandler) runWrite(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	defer ah.synchronizer.FileMux.Unlock()
	ah.handler.Handle(ctx, conn, req)
	if enableLogging {
		log.Println("Message processing unlocked for", req.Method)
	}
}
