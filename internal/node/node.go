// Package node is the endpoint clients measure against. One TCP port carries both
// the HTTPS control plane and the long-connection data plane; every client holds
// an invite, and every count the node keeps has a hard limit.
package node

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/netx"
	"github.com/Lynthar/ConnVerifier/internal/protocol"
)

const (
	handshakeTimeout = 10 * time.Second // accept to TLS handshake or HELLO done
	writeTimeout     = 5 * time.Second
	maxPendingPerIP  = 32
	maxPendingTotal  = 1024
	maxHTTPPerIP     = 8
	maxHTTPTotal     = 512
	requestRate      = 10 // control-plane requests per second per source address
	statsInterval    = 10 * time.Second
)

type Config struct {
	listen          string
	listenUDP       string // "" means the same address as listen
	stateDir        string
	maxConns        int
	maxSessions     int
	maxLoadSessions int
	logConnections  bool
}

// RegisterFlags binds cfg to fs under the command-line names and defaults.
func (cfg *Config) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&cfg.listen, "listen", ":7443", "TCP address to listen on (control plane and data plane share it)")
	fs.StringVar(&cfg.listenUDP, "listen-udp", "", "UDP address for STAMP (default: the -listen address)")
	fs.StringVar(&cfg.stateDir, "state-dir", DefaultStateDir(), "directory holding the node key and invites")
	fs.IntVar(&cfg.maxConns, "max-conns", 20000, "live data-plane connections across all sessions")
	fs.IntVar(&cfg.maxSessions, "max-sessions", 64, "sessions across all invites")
	fs.IntVar(&cfg.maxLoadSessions, "max-load-sessions", 1, "sessions running the load check at once (0: offer no load); more than one share this node's bandwidth")
	fs.BoolVar(&cfg.logConnections, "log-connections", false, "log each session with its invite label and client address")
}

// Validate reports the first setting that is out of range.
func (cfg Config) Validate() error {
	switch {
	case cfg.listen == "":
		return errors.New("listen must not be empty")
	case cfg.stateDir == "":
		return errors.New("state-dir must not be empty")
	case cfg.maxConns < 1 || cfg.maxConns > protocol.MaxConnections:
		return fmt.Errorf("max-conns must be 1 to %d", protocol.MaxConnections)
	case cfg.maxSessions < 1:
		return errors.New("max-sessions must be positive")
	case cfg.maxLoadSessions < 0:
		return errors.New("max-load-sessions must not be negative")
	}
	return nil
}

type counters struct {
	rejectedBusy, rejectedQuota, rejectedAuth, rejectedVersion, refusedPending, refusedHTTP atomic.Uint64
}

type server struct {
	cfg       Config
	version   string
	tls       *tls.Config
	store     *store
	pending   *pending
	httpConns *pending
	requests  *requestLimiter
	stats     counters
	udp       bool // a STAMP reflector is running
	wg        sync.WaitGroup
}

// Serve listens on the configured addresses and runs the node until ctx ends.
// version is reported to clients.
func Serve(ctx context.Context, cfg Config, version string) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	ln, err := netx.Listen(ctx, cfg.listen, 0)
	if err != nil {
		return fmt.Errorf("listen failed: %w", err)
	}
	udp := cfg.listenUDP
	if udp == "" {
		udp = cfg.listen
	}
	pc, err := netx.ListenPacket(ctx, udp)
	if err != nil {
		ln.Close()
		return fmt.Errorf("listen failed: %w", err)
	}
	return ServeOn(ctx, ln, pc, cfg, version)
}

// ServeOn runs the node on ln, and its STAMP reflector on pc unless pc is nil,
// until ctx ends, then closes both. The caller must have created ln through netx,
// or accepted connections may carry keepalive.
func ServeOn(ctx context.Context, ln net.Listener, pc net.PacketConn, cfg Config, version string) error {
	closeAll := func() {
		ln.Close()
		if pc != nil {
			pc.Close()
		}
	}
	cert, err := LoadIdentity(cfg.stateDir)
	if err != nil {
		closeAll()
		return err
	}
	if _, err := loadInvites(cfg.stateDir); err != nil {
		closeAll()
		return err
	}
	pin := protocol.Pin(cert.Leaf)
	udp := "off"
	if pc != nil {
		udp = pc.LocalAddr().String()
	}
	log.Printf("node listening on %s udp=%s key=%s max_conns=%d max_sessions=%d state=%s",
		ln.Addr(), udp, protocol.EncodeID(pin[:]), cfg.maxConns, cfg.maxSessions, cfg.stateDir)
	newServer(cfg, version, cert, pc != nil).serve(ctx, ln, pc)
	return nil
}

func newServer(cfg Config, version string, cert tls.Certificate, udp bool) *server {
	now := time.Now()
	return &server{
		cfg:       cfg,
		version:   version,
		tls:       protocol.ServerTLS(cert),
		store:     newStore(cfg.maxConns, cfg.maxSessions, cfg.maxLoadSessions, now),
		pending:   newPending(maxPendingPerIP, maxPendingTotal),
		httpConns: newPending(maxHTTPPerIP, maxHTTPTotal),
		requests:  newRequestLimiter(requestRate),
		udp:       udp,
	}
}

// serve accepts on ln until ctx ends, then closes every connection with a reason
// and waits for its goroutines.
func (s *server) serve(ctx context.Context, ln net.Listener, pc net.PacketConn) {
	httpLn := newConnListener(ln.Addr())
	hs := &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: handshakeTimeout,
		ReadTimeout:       handshakeTimeout,
		WriteTimeout:      handshakeTimeout,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    8 << 10,
		HTTP2:             &http.HTTP2Config{MaxReceiveBufferPerConnection: h2ReceiveSize, MaxReceiveBufferPerStream: h2ReceiveSize, MaxReadFrameSize: h2MaxFrame},
		ConnContext:       func(ctx context.Context, c net.Conn) context.Context { return context.WithValue(ctx, connKey{}, c) },
		ConnState: func(c net.Conn, st http.ConnState) {
			if st == http.StateClosed || st == http.StateHijacked {
				s.httpConns.release(remoteIP(c))
			}
		},
	}
	roundRobin(hs)
	s.wg.Add(3)
	go func() { defer s.wg.Done(); hs.Serve(httpLn) }()
	go func() { defer s.wg.Done(); s.expireLoop(ctx) }()
	go func() { defer s.wg.Done(); s.statsLoop(ctx) }()
	go func() { <-ctx.Done(); ln.Close() }()
	if pc != nil {
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.reflect(ctx, pc) }()
		go func() { <-ctx.Done(); pc.Close() }()
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Printf("accept failed: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		addr := remoteIP(conn)
		if !s.pending.acquire(addr, s.store.loadAllowance(addr)) {
			s.stats.refusedPending.Add(1)
			conn.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.route(ctx, conn, addr, httpLn)
		}()
	}

	// CLOSE goes out before the HTTP shutdown, which can wait a second for an HTTP/2 peer.
	closeAll(s.store.close(), protocol.ReasonShuttingDown)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hs.Shutdown(shutdownCtx)
	s.wg.Wait()
	l := s.store.load()
	log.Printf("shutdown: sessions=%d connections=%d", l.Sessions, l.Connections)
}

func (s *server) expireLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			conns := s.store.end(func(sess *session) bool { return !now.Before(sess.expires) })
			closeAll(conns, protocol.ReasonSessionEnded)
		}
	}
}

func (s *server) statsLoop(ctx context.Context) {
	t := time.NewTicker(statsInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l := s.store.load()
			log.Printf("stats sessions=%d connections=%d rejected_busy=%d rejected_quota=%d rejected_auth=%d refused_unauthenticated=%d refused_https=%d",
				l.Sessions, l.Connections, s.stats.rejectedBusy.Load(), s.stats.rejectedQuota.Load(),
				s.stats.rejectedAuth.Load(), s.stats.refusedPending.Load(), s.stats.refusedHTTP.Load())
		}
	}
}

func (s *server) nodeInfo() protocol.NodeInfo {
	return protocol.NodeInfo{Version: s.version, Load: s.store.load()}
}

// remoteIP is the peer address with any IPv4-in-IPv6 mapping removed, so a client
// cannot get two budgets from one dual-stack listener.
func remoteIP(c net.Conn) netip.Addr {
	if ap, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.Addr{}
}
