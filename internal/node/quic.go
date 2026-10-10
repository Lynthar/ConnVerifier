package node

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/Lynthar/ConnVerifier/internal/protocol"
)

const (
	// quicHandshakeTimeout matches the client's: a node that gave up sooner would
	// cut handshakes the client still waits for across a loaded queue.
	quicHandshakeTimeout = 15 * time.Second
	// Above this many new handshakes a second from one address the node asks for
	// a Retry first, proving the address; a load check's foreign probes are 20.
	quicHandshakeRate = 50
)

// quicConns counts the HTTP/3 connections of each address, from their first
// Initial until they close.
type quicConns struct {
	mu   sync.Mutex
	live map[netip.Addr]int
}

// serveQUIC serves the load endpoints over HTTP/3 on pc until ctx ends. Only an
// address holding a live QUIC load grant gets a handshake, and at most its grants'
// load connections plus maxHTTPPerIP of them at once — the slack the TCP side
// gives too, for connections whose close is still queued behind the load. No
// keepalive, no 0-RTT, no datagrams, the receive windows of the HTTP/2 side.
func (s *server) serveQUIC(ctx context.Context, pc net.PacketConn) {
	handshakes := newRequestLimiter(quicHandshakeRate)
	tr := &quic.Transport{
		Conn: pc,
		VerifySourceAddress: func(a net.Addr) bool {
			return !handshakes.allow(addrIP(a), time.Now())
		},
		// Refusing here, before the handshake, lets a client count the connection
		// as not opened instead of losing a stream it already started.
		ConnContext: func(cctx context.Context, info *quic.ClientInfo) (context.Context, error) {
			ip := addrIP(info.RemoteAddr)
			allowance := s.store.quicAllowance(ip)
			if allowance == 0 || !s.quic.acquire(ip, allowance+maxHTTPPerIP) {
				return nil, errNoQUICRoom
			}
			context.AfterFunc(cctx, func() { s.quic.release(ip) })
			return cctx, nil
		},
	}
	conf := &quic.Config{
		HandshakeIdleTimeout:       quicHandshakeTimeout,
		MaxStreamReceiveWindow:     h2ReceiveSize,
		MaxConnectionReceiveWindow: h2ReceiveSize,
	}
	ln, err := tr.Listen(http3.ConfigureTLSConfig(s.tls.Clone()), conf)
	if err != nil {
		return
	}
	hs := &http3.Server{Handler: s.loadRoutes(), IdleTimeout: time.Minute, MaxHeaderBytes: 8 << 10}
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-stopped:
		}
		hs.Close()
		ln.Close()
		tr.Close()
	}()
	hs.ServeListener(ln)
	close(stopped)
}

var errNoQUICRoom = errors.New("no QUIC load grant with room for this address")

func (q *quicConns) acquire(ip netip.Addr, allowance int) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.live[ip] >= allowance {
		return false
	}
	q.live[ip]++
	return true
}

func (q *quicConns) release(ip netip.Addr) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.live[ip]--; q.live[ip] <= 0 {
		delete(q.live, ip)
	}
}

// loadRoutes serves only the load endpoints: HTTP/3 carries load, the control
// plane stays on TCP.
func (s *server) loadRoutes() *http.ServeMux {
	load := http.NewServeMux()
	load.HandleFunc("GET "+protocol.PathSessions+"/{id}"+protocol.LoadSmall, s.loadSmall)
	load.HandleFunc("GET "+protocol.PathSessions+"/{id}"+protocol.LoadLarge, s.loadLarge)
	load.HandleFunc("POST "+protocol.PathSessions+"/{id}"+protocol.LoadUpload, s.loadUpload)
	return load
}

// addrIP is a UDP peer's address with any IPv4-in-IPv6 mapping removed.
func addrIP(a net.Addr) netip.Addr {
	if ap, err := netip.ParseAddrPort(a.String()); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.Addr{}
}
