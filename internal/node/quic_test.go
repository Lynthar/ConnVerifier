package node

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/Lynthar/ConnVerifier/internal/memnet"
	"github.com/Lynthar/ConnVerifier/internal/protocol"
)

// memnet's datagram clients all come from this address.
var memClient = netip.MustParseAddr("192.0.2.200")

// quicServer runs s with HTTP/3 on an in-memory port and returns a function that
// completes one QUIC handshake from a fresh client socket, or fails.
func quicServer(t *testing.T, s *server) func() (*quic.Conn, error) {
	t.Helper()
	s.quicPort = 7444
	qc := memnet.NewPacketConn("192.0.2.10:7444")
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { s.serve(ctx, memnet.NewListener(), nil, qc); close(stopped) }()
	t.Cleanup(func() { cancel(); <-stopped })
	return func() (*quic.Conn, error) {
		pc := qc.Dial().PacketConn()
		tr := &quic.Transport{Conn: pc}
		t.Cleanup(func() { tr.Close(); pc.Close() })
		tc := protocol.ClientTLS(protocol.Pin(s.tls.Certificates[0].Leaf))
		tc.NextProtos = []string{http3.NextProtoH3}
		dctx, done := context.WithTimeout(ctx, 2*time.Second)
		defer done()
		return tr.Dial(dctx, qc.LocalAddr(), tc, &quic.Config{})
	}
}

// Only an address holding a live QUIC load grant gets a handshake; a TCP load
// grant from the same address does not open the port.
func TestQUICOnlyForGrantedAddresses(t *testing.T) {
	s, _ := testServer(t, 10)
	s.store.maxLoadSessions = 2
	dial := quicServer(t, s)
	if _, err := dial(); err == nil {
		t.Fatal("handshake without any grant")
	}
	s.store.create("tcp", DefaultInviteLimits, loadWant(1<<20, 4), memClient, false, time.Now())
	if _, err := dial(); err == nil {
		t.Fatal("handshake with a TCP load grant only")
	}
	s.store.create("quic", DefaultInviteLimits, loadWant(1<<20, 4), memClient, true, time.Now())
	c, err := dial()
	if err != nil {
		t.Fatalf("handshake with a QUIC load grant: %v", err)
	}
	c.CloseWithError(0, "")
}

// QUIC connections from an address are capped at its grants' load connections
// plus the slack for closing ones, refused before the handshake beyond that; one
// closing makes room for the next.
func TestQUICConnectionsCapped(t *testing.T) {
	s, _ := testServer(t, 10)
	dial := quicServer(t, s)
	s.store.create("quic", DefaultInviteLimits, loadWant(1<<20, 2), memClient, true, time.Now())
	var live []*quic.Conn
	for range 2 + maxHTTPPerIP {
		c, err := dial()
		if err != nil {
			t.Fatalf("handshake %d within the cap: %v", len(live)+1, err)
		}
		live = append(live, c)
	}
	if _, err := dial(); err == nil {
		t.Fatal("a handshake beyond the cap")
	}
	live[0].CloseWithError(0, "")
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := dial()
		if err == nil {
			c.CloseWithError(0, "")
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no room after a connection closed")
		}
	}
}

// A load session that asks for QUIC is told the node's QUIC port, and a node
// without HTTP/3 grants TCP, as an older node would.
func TestQUICGrant(t *testing.T) {
	for _, port := range []int{7444, 0} {
		t.Run(fmt.Sprint(port), func(t *testing.T) {
			s, inv := testServer(t, 10)
			s.quicPort = port
			body := `{"client":{"name":"t","version":"v"},"check":"load","transport":"quic",` +
				`"want":{"connections":1,"dial_rate":1,"duration_s":60,"idle_timeout_s":60,"load_bytes":1000000,"load_connections":8}}`
			w := apiRequest(t, s, "POST", protocol.PathSessions, protocol.EncodeID(inv.Token[:]), body)
			var resp protocol.SessionResponse
			if w.Code != http.StatusCreated || protocol.DecodeJSON(w.Body, &resp) != nil || resp.Load == nil {
				t.Fatalf("create = %d %s", w.Code, w.Body)
			}
			switch {
			case port > 0 && (resp.Load.QUIC == nil || resp.Load.QUIC.Port != port):
				t.Fatalf("grant %+v; want QUIC on %d", resp.Load, port)
			case port == 0 && resp.Load.QUIC != nil:
				t.Fatalf("grant %+v from a node without HTTP/3", resp.Load)
			}
		})
	}
}
