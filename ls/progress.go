package ls

import (
	"context"
	"log"
	"sync"

	"github.com/arduino/arduino-language-server/streams"
	"go.bug.st/lsp"
)

type progressProxyHandler struct {
	conn               *lsp.Server
	mux                sync.Mutex
	actionRequiredCond *sync.Cond
	proxies            map[string]*progressProxy
}

type progressProxyStatus int

const (
	progressProxyNew progressProxyStatus = iota
	progressProxyCreated
	progressProxyBegin
	progressProxyReport
	progressProxyEnd
)

type progressProxy struct {
	currentStatus  progressProxyStatus
	requiredStatus progressProxyStatus
	beginReq       *lsp.WorkDoneProgressBegin
	reportReq      *lsp.WorkDoneProgressReport
	endReq         *lsp.WorkDoneProgressEnd
}

// newProgressProxy creates a new ProgressProxyHandler and returns its pointer
func newProgressProxy(conn *lsp.Server) *progressProxyHandler {
	res := &progressProxyHandler{
		conn:    conn,
		proxies: map[string]*progressProxy{},
	}
	res.actionRequiredCond = sync.NewCond(&res.mux)
	go func() {
		defer streams.CatchAndLogPanic()
		res.handlerLoop()
	}()
	return res
}

func (p *progressProxyHandler) handlerLoop() {
	p.mux.Lock()
	defer p.mux.Unlock()

	for {
		p.actionRequiredCond.Wait()

		for id, proxy := range p.proxies {
			for proxy.currentStatus != proxy.requiredStatus {
				p.handleProxy(id, proxy)
			}
		}

		// Cleanup ended proxies
		for id, proxy := range p.proxies {
			if proxy.currentStatus == progressProxyEnd {
				delete(p.proxies, id)
			}
		}
	}
}

func (p *progressProxyHandler) handleProxy(id string, proxy *progressProxy) {
	switch proxy.currentStatus {
	case progressProxyNew:
		p.mux.Unlock()
		respErr, err := p.conn.WindowWorkDoneProgressCreate(context.Background(), &lsp.WorkDoneProgressCreateParams{
			Token: lsp.EncodeMessage(id),
		})
		if err != nil {
			log.Printf("ProgressHandler: error creating token %s: %v", id, err)
			break
		}
		if respErr != nil {
			log.Printf("ProgressHandler: error creating token %s: %v", id, respErr.AsError())
			break
		}
		p.mux.Lock()
		proxy.currentStatus = progressProxyCreated

	case progressProxyCreated:
		err := p.conn.Progress(&lsp.ProgressParams{
			Token: lsp.EncodeMessage(id),
			Value: lsp.EncodeMessage(proxy.beginReq),
		})

		proxy.beginReq = nil
		if err != nil {
			log.Printf("ProgressHandler: error sending begin req token %s: %v", id, err)
		} else {
			proxy.currentStatus = progressProxyBegin
		}

	case progressProxyBegin:
		if proxy.requiredStatus == progressProxyReport {
			err := p.conn.Progress(&lsp.ProgressParams{
				Token: lsp.EncodeMessage(id),
				Value: lsp.EncodeMessage(proxy.reportReq),
			})

			proxy.reportReq = nil
			if err != nil {
				log.Printf("ProgressHandler: error sending report req token %s: %v", id, err)
			} else {
				proxy.requiredStatus = progressProxyBegin
			}

		} else if proxy.requiredStatus == progressProxyEnd {
			err := p.conn.Progress(&lsp.ProgressParams{
				Token: lsp.EncodeMessage(id),
				Value: lsp.EncodeMessage(proxy.endReq),
			})

			proxy.endReq = nil
			if err != nil {
				log.Printf("ProgressHandler: error sending end req token %s: %v", id, err)
			} else {
				proxy.currentStatus = progressProxyEnd
			}

		}
	}
}

func (p *progressProxyHandler) Create(id string) {
	p.mux.Lock()
	defer p.mux.Unlock()

	if _, opened := p.proxies[id]; opened {
		// Already created
		return
	}

	p.proxies[id] = &progressProxy{
		currentStatus:  progressProxyNew,
		requiredStatus: progressProxyCreated,
	}
	p.actionRequiredCond.Broadcast()
}

func (p *progressProxyHandler) Begin(id string, req *lsp.WorkDoneProgressBegin) {
	p.mux.Lock()
	defer p.mux.Unlock()

	proxy, ok := p.proxies[id]
	if !ok {
		return
	}
	if proxy.requiredStatus == progressProxyReport {
		return
	}
	if proxy.requiredStatus == progressProxyEnd {
		return
	}

	proxy.beginReq = req
	proxy.requiredStatus = progressProxyBegin
	p.actionRequiredCond.Broadcast()
}

func (p *progressProxyHandler) Report(id string, req *lsp.WorkDoneProgressReport) {
	p.mux.Lock()
	defer p.mux.Unlock()

	proxy, ok := p.proxies[id]
	if !ok {
		return
	}
	if proxy.requiredStatus == progressProxyEnd {
		return
	}
	proxy.reportReq = req
	proxy.requiredStatus = progressProxyReport
	p.actionRequiredCond.Broadcast()
}

func (p *progressProxyHandler) End(id string, req *lsp.WorkDoneProgressEnd) {
	p.mux.Lock()
	defer p.mux.Unlock()

	proxy, ok := p.proxies[id]
	if !ok {
		return
	}

	proxy.endReq = req
	proxy.requiredStatus = progressProxyEnd
	p.actionRequiredCond.Broadcast()
}

func (p *progressProxyHandler) Shutdown() {
	p.mux.Lock()
	defer p.mux.Unlock()

	for id, proxy := range p.proxies {
		err := p.conn.Progress(&lsp.ProgressParams{
			Token: lsp.EncodeMessage(id),
			Value: lsp.EncodeMessage(&lsp.WorkDoneProgressEnd{
				Message: "Shutdown",
			}),
		})

		proxy.endReq = nil
		if err != nil {
			log.Printf("ProgressHandler: error sending end req token %s: %v", id, err)
		} else {
			proxy.currentStatus = progressProxyEnd
			proxy.requiredStatus = progressProxyEnd
		}
	}

	p.actionRequiredCond.Broadcast()
}
