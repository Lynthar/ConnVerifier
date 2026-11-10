package dns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"runtime"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/netenv"
	"github.com/Lynthar/ConnVerifier/internal/netx"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

// reply answers the nth query (from 1) as a resolver would: the question kept,
// the code set, the records left out; nil sends nothing.
type reply func(n int, q []byte) []byte

func answerWith(rcode byte) reply {
	return func(_ int, q []byte) []byte {
		a := append([]byte(nil), q[:17]...)
		a[2] |= 0x80
		a[3] = a[3]&0xf0 | rcode
		a[11] = 0
		return a
	}
}

// fakeResolver serves DNS on a loopback UDP port until the test ends.
func fakeResolver(t *testing.T, addr string, r reply) int {
	pc, err := netx.ListenPacket(t.Context(), addr)
	if err != nil {
		t.Skipf("no %s here: %v", addr, err)
	}
	t.Cleanup(func() { pc.Close() })
	var n atomic.Int32
	go func() {
		b := make([]byte, 1500)
		for {
			k, from, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			if a := r(int(n.Add(1)), b[:k]); a != nil {
				pc.WriteTo(a, from)
			}
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

func metric(c result.Check, id string) float64 {
	for _, m := range c.Metrics {
		if m.ID == id && m.Value != nil {
			return *m.Value
		}
	}
	return -1
}

func testConfig(port int, list ...string) Config {
	return Config{
		resolvers: func() ([]netip.Addr, error) {
			var out []netip.Addr
			for _, a := range list {
				out = append(out, netip.MustParseAddr(a))
			}
			return out, nil
		},
		route:    func(context.Context, string) (netenv.Route, error) { return netenv.Route{Kind: netenv.KindWired}, nil },
		port:     port,
		interval: time.Millisecond,
		wait:     300 * time.Millisecond,
	}
}

func TestRun(t *testing.T) {
	every4th := func(n int, q []byte) []byte {
		if n%4 == 0 {
			return nil
		}
		return answerWith(0)(n, q)
	}
	for _, tt := range []struct {
		name          string
		reply         reply
		status        result.Status
		replies, errs float64
		key           string
	}{
		{"answers", answerWith(0), result.Pass, 20, 0, ""},
		{"drops some", every4th, result.Warn, 15, 0, "dns.inference.unanswered"},
		{"server failure", answerWith(2), result.Warn, 20, 20, "dns.inference.error_replies"},
		{"silent", func(int, []byte) []byte { return nil }, result.Warn, 0, 0, "dns.inference.unanswered"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := runtime.NumGoroutine()
			port := fakeResolver(t, "127.0.0.1:0", tt.reply)
			checks := Run(t.Context(), testConfig(port, "127.0.0.1"))
			if len(checks) != 2 || checks[1].Status != result.Skip || !slices.Contains(messageKeys(checks[1]), "dns.skip.no_resolver") {
				t.Fatalf("checks %+v, want the IPv4 resolver and an IPv6 SKIP", checks)
			}
			c := checks[0]
			if c.Status != tt.status || metric(c, "sent") != 20 || metric(c, "replies") != tt.replies || metric(c, "error_replies") != tt.errs ||
				c.Params["resolver"] != 1 || c.Params["resolver_class"] != netenv.ClassLoopback || c.Path.Family != "ipv4" {
				t.Fatalf("status %s sent %v replies %v errors %v params %v", c.Status, metric(c, "sent"), metric(c, "replies"), metric(c, "error_replies"), c.Params)
			}
			if tt.key != "" && !slices.Contains(messageKeys(c), tt.key) {
				t.Fatalf("messages %v, want %s", messageKeys(c), tt.key)
			}
			for deadline := time.Now().Add(time.Second); runtime.NumGoroutine() > before+1 && time.Now().Before(deadline); {
				time.Sleep(time.Millisecond)
			}
			if n := runtime.NumGoroutine(); n > before+1 { // +1: the fake resolver, stopped at cleanup
				t.Fatalf("%d goroutines left over", n-before-1)
			}
		})
	}
}

// Each resolver is a check of its own, numbered by its place in the host's list,
// and one reached through a tunnel says so.
func TestRunResolvers(t *testing.T) {
	port := fakeResolver(t, "[::1]:0", answerWith(0))
	fakeResolver(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), answerWith(0)) // one port: the config has one
	cfg := testConfig(port, "::1", "127.0.0.1")
	cfg.route = func(_ context.Context, hostPort string) (netenv.Route, error) {
		return netenv.Route{Kind: map[bool]string{true: netenv.KindTunnel, false: netenv.KindWired}[hostPort[0] == '[']}, nil
	}
	checks := Run(t.Context(), cfg)
	if len(checks) != 2 || checks[0].Path.Family != "ipv4" || checks[0].Params["resolver"] != 2 ||
		checks[1].Path.Family != "ipv6" || checks[1].Params["resolver"] != 1 || !slices.Contains(messageKeys(checks[1]), "dns.not_proven.tunnel") {
		t.Fatalf("checks %+v", checks)
	}
}

func TestRunWithoutConfig(t *testing.T) {
	cfg := testConfig(0)
	cfg.resolvers = func() ([]netip.Addr, error) { return nil, errors.New("no scutil") }
	for _, c := range Run(t.Context(), cfg) {
		if c.Status != result.Unsupported || !slices.Contains(messageKeys(c), "dns.unsupported.no_config") {
			t.Fatalf("%s: %s %v, want UNSUPPORTED for the missing configuration", c.Path.Family, c.Status, messageKeys(c))
		}
	}
}

// A port with nothing on it answers "port unreachable": the queries are lost and
// the check says the resolver is not usable, without ending in ERROR.
func TestRunNothingListening(t *testing.T) {
	pc, err := netx.ListenPacket(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()
	c := Run(t.Context(), testConfig(port, "127.0.0.1"))[0]
	if c.Status != result.Warn || metric(c, "replies") != 0 {
		t.Fatalf("status %s replies %v, want WARN with none", c.Status, metric(c, "replies"))
	}
}

func TestRunInterrupted(t *testing.T) {
	port := fakeResolver(t, "127.0.0.1:0", answerWith(0))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c := Run(ctx, testConfig(port, "127.0.0.1"))[0]
	if c.Status != result.Invalid || !slices.Contains(messageKeys(c), "dns.warning.interrupted") {
		t.Fatalf("status %s messages %v, want INVALID for the interruption", c.Status, messageKeys(c))
	}
}
