package gateway

import (
	"net"
	"net/netip"
	"runtime"
	"testing"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/netx"
)

// Echoes to this host itself are answered wherever unprivileged ICMP exists; the
// sequence numbers cross 65535 to show they wrap.
func TestICMPLoopback(t *testing.T) {
	for _, a := range []string{"127.0.0.1", "::1"} {
		p, err := newICMP(netip.MustParseAddr(a))
		if runtime.GOOS == "windows" {
			if err == nil {
				p.close()
				t.Fatal("Windows opened an unprivileged ICMP socket")
			}
			continue
		}
		if err != nil {
			t.Logf("%s: no unprivileged ICMP here: %v", a, err)
			continue
		}
		rs := sample(t.Context(), p, 1<<16-2, 4, time.Millisecond)
		p.close()
		for i, r := range rs {
			if !r.ok || r.rtt <= 0 || r.rtt > replyWait {
				t.Errorf("%s probe %d: %+v, want an answer", a, i, r)
			}
		}
	}
}

func TestTCPProbe(t *testing.T) {
	ln, err := netx.Listen(t.Context(), "127.0.0.1:0", 0)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	gw, port := netip.MustParseAddr("127.0.0.1"), ln.Addr().(*net.TCPAddr).Port
	if r := newTCP(gw, port).probe(t.Context(), 0); !r.ok {
		t.Fatalf("handshake with a listener: %+v, want an answer", r)
	}
	ln.Close()
	if runtime.GOOS == "windows" {
		return // Windows retries a refused connection longer than replyWait
	}
	if r := newTCP(gw, port).probe(t.Context(), 0); !r.ok {
		t.Fatalf("closed port: %+v, want its reset to count as an answer", r)
	}
}
