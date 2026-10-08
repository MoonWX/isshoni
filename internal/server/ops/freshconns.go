package ops

import (
	"net"
	"net/http"
	"sync"
)

// freshConns are the connections of one http.Server that have not sent a complete request header yet
// (http.StateNew): a client's spare connection, a port scanner, a request cut in half.
//
// http.Server.Shutdown waits for such a connection until it is 5 s old, longer than a shutdown step may take
// (04 §6.4). So the servers of this package (the admin socket, the metrics listener) close them themselves when
// their Shutdown begins. No answer is lost: a connection in this state has no request to answer.
//
// The zero value is ready to use: track is the server's ConnState hook, closeAll the first step of its Shutdown.
type freshConns struct {
	mu      sync.Mutex
	closing bool // closeAll ran: nothing is tracked any more
	conns   map[net.Conn]struct{}
}

// track is the http.Server.ConnState hook: it follows each connection until its first request header is in. Every
// connection starts in StateNew and leaves it for good with that header, or when it closes.
func (f *freshConns) track(c net.Conn, state http.ConnState) {
	f.mu.Lock()
	if state != http.StateNew {
		delete(f.conns, c)
		f.mu.Unlock()
		return
	}
	closing := f.closing
	if !closing {
		if f.conns == nil {
			f.conns = make(map[net.Conn]struct{})
		}
		f.conns[c] = struct{}{}
	}
	f.mu.Unlock()
	if closing {
		_ = c.Close() // accepted between closeAll and the moment the listener closed
	}
}

// closeAll closes every connection that has not sent a request yet, and makes track close the ones that are still
// accepted afterwards.
func (f *freshConns) closeAll() {
	f.mu.Lock()
	f.closing = true
	conns := f.conns
	f.conns = nil
	f.mu.Unlock()
	for c := range conns {
		_ = c.Close()
	}
}
