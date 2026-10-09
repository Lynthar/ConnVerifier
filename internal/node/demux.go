package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/protocol"
)

const tlsHandshakeRecord = 0x16

// pending counts connections per source address against a limit: unproven ones
// (accept to TLS handshake or HELLO) and, separately, open control connections.
// An address with a load grant may go over both limits by its allowance.
type pending struct {
	mu              sync.Mutex
	perIP           map[netip.Addr]int
	total           int
	maxPerIP, limit int
}

func newPending(maxPerIP, limit int) *pending {
	return &pending{perIP: make(map[netip.Addr]int), maxPerIP: maxPerIP, limit: limit}
}

func (p *pending) acquire(a netip.Addr, allowance int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.perIP[a] >= p.maxPerIP+allowance || p.total >= p.limit+allowance {
		return false
	}
	p.perIP[a]++
	p.total++
	return true
}

func (p *pending) release(a netip.Addr) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.total--
	if p.perIP[a]--; p.perIP[a] <= 0 {
		delete(p.perIP, a)
	}
}

// peekedConn returns the byte used for routing before the rest of the stream;
// deadlines, writes and close go to the underlying connection.
type peekedConn struct {
	net.Conn
	r io.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// route reads the first byte within the handshake budget and hands the
// connection to TLS or to the data plane; anything else is closed without a reply.
func (s *server) route(ctx context.Context, conn net.Conn, addr netip.Addr, httpLn *connListener) {
	var once sync.Once
	release := func() { once.Do(func() { s.pending.release(addr) }) }
	defer release()

	deadline := time.Now().Add(handshakeTimeout)
	conn.SetDeadline(deadline)
	var first [1]byte
	if _, err := io.ReadFull(conn, first[:]); err != nil {
		conn.Close()
		return
	}
	pc := &peekedConn{Conn: conn, r: io.MultiReader(bytes.NewReader(first[:]), conn)}

	switch first[0] {
	case tlsHandshakeRecord:
		tc := tls.Server(pc, s.tls)
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return
		}
		release()
		if !s.httpConns.acquire(addr, s.store.loadAllowance(addr)) {
			s.stats.refusedHTTP.Add(1)
			conn.Close()
			return
		}
		conn.SetDeadline(time.Time{}) // the HTTP server sets its own from here
		if !httpLn.deliver(tc) {
			s.httpConns.release(addr) // never reached the server, so no ConnState will
		}
	case protocol.MagicByte:
		conn.SetWriteDeadline(time.Time{})
		s.serveData(pc, release)
	default:
		conn.Close()
	}
}

// connListener feeds connections whose TLS handshake is done to an http.Server.
type connListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
	addr  net.Addr
}

func newConnListener(addr net.Addr) *connListener {
	return &connListener{conns: make(chan net.Conn), done: make(chan struct{}), addr: addr}
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *connListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *connListener) Addr() net.Addr { return l.addr }

// deliver hands c to the server, or closes it and reports false once the
// listener is closed.
func (l *connListener) deliver(c net.Conn) bool {
	select {
	case l.conns <- c:
		return true
	case <-l.done:
		c.Close()
		return false
	}
}
