package gateway

import (
	"context"
	"errors"
	"net/netip"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/netenv"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

// fakeProber answers as answer says, at once; a probe after the run ended is cut.
type fakeProber struct{ answer func(seq int) reply }

func (f fakeProber) probe(ctx context.Context, seq int) reply {
	if ctx.Err() != nil {
		return reply{cut: true}
	}
	return f.answer(seq)
}

func (fakeProber) close() {}

var (
	all     = func(int) reply { return reply{rtt: time.Millisecond, ok: true} }
	silent  = func(int) reply { return reply{} }
	tenthOK = func(seq int) reply {
		if seq%10 == 0 {
			return reply{}
		}
		return all(seq)
	}
)

func metric(c result.Check, id string) float64 {
	for _, m := range c.Metrics {
		if m.ID == id && m.Value != nil {
			return *m.Value
		}
	}
	return -1
}

func TestRun(t *testing.T) {
	gw := netip.MustParseAddr("192.168.1.1")
	route := func(family string) (netip.Addr, error) { return gw, nil }
	icmpAnswers := func(answer func(int) reply) func(netip.Addr) (prober, error) {
		return func(netip.Addr) (prober, error) { return fakeProber{answer}, nil }
	}
	noICMP := func(netip.Addr) (prober, error) { return nil, errors.New("permission denied") }
	tcpOn := func(open int) func(netip.Addr, int) prober {
		return func(_ netip.Addr, port int) prober {
			if port == open {
				return fakeProber{all}
			}
			return fakeProber{silent}
		}
	}
	tests := []struct {
		name     string
		gateway  func(string) (netip.Addr, error)
		icmp     func(netip.Addr) (prober, error)
		tcp      func(netip.Addr, int) prober
		status   result.Status
		protocol string
		port     any
		sent     float64
		lost     float64
		key      string
	}{
		{"0a no default route", func(string) (netip.Addr, error) { return netip.Addr{}, netenv.ErrNoDefaultRoute },
			nil, nil, result.Skip, "", nil, -1, -1, "gateway.skip.no_default_route"},
		{"0b no gateway", func(string) (netip.Addr, error) { return netip.Addr{}, netenv.ErrNoGateway },
			nil, nil, result.Skip, "", nil, -1, -1, "gateway.skip.no_gateway"},
		{"0c routes unreadable", func(string) (netip.Addr, error) { return netip.Addr{}, errors.New("no table") },
			nil, nil, result.Unsupported, "", nil, -1, -1, "gateway.unsupported.no_routes"},
		{"ICMP answers", route, icmpAnswers(all), tcpOn(0), result.Pass, "icmp", nil, 100, 0, ""},
		{"ICMP loses some", route, icmpAnswers(tenthOK), tcpOn(0), result.Warn, "icmp", nil, 100, 10, "gateway.inference.loss_icmp"},
		{"ICMP silent, port 80 answers", route, icmpAnswers(silent), tcpOn(80), result.Pass, "tcp", 80, 100, 0, "gateway.not_proven.tcp"},
		{"no ICMP, port 53 answers", route, noICMP, tcpOn(53), result.Pass, "tcp", 53, 100, 0, "gateway.not_proven.tcp_no_icmp"},
		{"nothing answers", route, icmpAnswers(silent), tcpOn(0), result.Unsupported, "", nil, -1, -1, "gateway.unsupported.no_answer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := runtime.NumGoroutine()
			cfg := Config{gateway: tt.gateway, icmp: tt.icmp, tcp: tt.tcp, interval: time.Millisecond}
			checks := Run(t.Context(), cfg)
			if len(checks) != 2 || checks[0].Path.Family != "ipv4" || checks[1].Path.Family != "ipv6" {
				t.Fatalf("checks %+v, want one per family, IPv4 first", checks)
			}
			c := checks[0]
			if c.Status != tt.status || c.Path.Protocol != tt.protocol || c.Params["tcp_port"] != tt.port ||
				metric(c, "sent") != tt.sent || metric(c, "lost") != tt.lost {
				t.Fatalf("status %s protocol %q port %v sent %v lost %v; want %s %q %v %v %v",
					c.Status, c.Path.Protocol, c.Params["tcp_port"], metric(c, "sent"), metric(c, "lost"),
					tt.status, tt.protocol, tt.port, tt.sent, tt.lost)
			}
			if tt.key != "" && !slices.Contains(messageKeys(c), tt.key) {
				t.Fatalf("messages %v, want %s", messageKeys(c), tt.key)
			}
			if _, ok := c.Params["gateway_class"]; ok != (tt.protocol != "" || tt.key == "gateway.unsupported.no_answer") {
				t.Fatalf("gateway_class present %v in %v; want it once a gateway was found", ok, c.Params)
			}
			for deadline := time.Now().Add(time.Second); runtime.NumGoroutine() > before && time.Now().Before(deadline); {
				time.Sleep(time.Millisecond)
			}
			if n := runtime.NumGoroutine(); n > before {
				t.Fatalf("%d goroutines left over", n-before)
			}
		})
	}
}

// A run that ends before any answer leaves nothing to judge.
func TestRunInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cfg := Config{
		gateway:  func(string) (netip.Addr, error) { return netip.MustParseAddr("192.168.1.1"), nil },
		icmp:     func(netip.Addr) (prober, error) { return fakeProber{all}, nil },
		tcp:      func(netip.Addr, int) prober { return fakeProber{all} },
		interval: time.Millisecond,
	}
	for _, c := range Run(ctx, cfg) {
		if c.Status != result.Invalid || !slices.Contains(messageKeys(c), "gateway.warning.interrupted") {
			t.Fatalf("%s: status %s messages %v, want INVALID for the interruption", c.Path.Family, c.Status, messageKeys(c))
		}
	}
}
