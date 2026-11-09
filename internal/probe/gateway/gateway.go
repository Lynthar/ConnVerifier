// Package gateway runs the gateway check: round trip and loss from this host to
// the default gateway of each address family, over ICMP echo or, when that gets no
// answer, TCP handshakes. It needs no node.
package gateway

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/Lynthar/ConnVerifier/internal/netenv"
	"github.com/Lynthar/ConnVerifier/internal/netx"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

// ID is the check's ID.
const ID = "gateway"

// MethodVersion goes up with any change to the probing, a definition, the
// estimators or the status rules.
const MethodVersion = 1

const (
	probes   = 100
	interval = 50 * time.Millisecond // 20 a second
	// replyWait bounds every probe. A handshake must give up before the first SYN
	// retransmission, 1 s, or that second enters its round trip; ICMP waits as long,
	// so both methods count loss alike.
	replyWait = 900 * time.Millisecond
	echoBytes = 56 // ICMP echo data, as ping sends
)

// tcpPorts are tried in order when ICMP gets no answer.
var tcpPorts = []int{53, 80, 443}

// Config holds what tests replace; the zero value probes this host.
type Config struct {
	gateway  func(family string) (netip.Addr, error)
	icmp     func(gw netip.Addr) (prober, error)
	tcp      func(gw netip.Addr, port int) prober
	interval time.Duration
}

func (cfg Config) withDefaults() Config {
	if cfg.gateway == nil {
		cfg.gateway = netenv.DefaultGateway
	}
	if cfg.icmp == nil {
		cfg.icmp = newICMP
	}
	if cfg.tcp == nil {
		cfg.tcp = newTCP
	}
	if cfg.interval <= 0 {
		cfg.interval = interval
	}
	return cfg
}

// Run checks the default gateway of IPv4, then of IPv6; one family at a time, so
// neither's probes queue behind the other's.
func Run(ctx context.Context, cfg Config) []result.Check {
	cfg = cfg.withDefaults()
	return []result.Check{run(ctx, cfg, "ipv4"), run(ctx, cfg, "ipv6")}
}

func newCheck(family string) result.Check {
	return result.Check{
		ID: ID, MethodVersion: MethodVersion,
		Path: result.Path{Family: family},
		Params: map[string]any{
			"probes":        probes,
			"rate_per_s":    int(time.Second / interval),
			"reply_wait_ms": replyWait.Milliseconds(),
		},
		Metrics: []result.Metric{},
	}
}

func run(ctx context.Context, cfg Config, family string) (c result.Check) {
	start := time.Now()
	c = newCheck(family)
	defer func() { c.ElapsedMs = time.Since(start).Milliseconds() }()
	gw, err := cfg.gateway(family)
	switch {
	case errors.Is(err, netenv.ErrNoDefaultRoute):
		skip(&c, "gateway.skip.no_default_route")
		return c
	case errors.Is(err, netenv.ErrNoGateway):
		skip(&c, "gateway.skip.no_gateway")
		return c
	case err != nil:
		c.Status = result.Unsupported
		c.Warnings = append(c.Warnings, msg("gateway.unsupported.no_routes", map[string]any{"error": err.Error()}))
		return c
	}
	c.Params["gateway_class"] = netenv.AddrClass(gw)
	finish(&c, measure(ctx, cfg, gw), ctx.Err() != nil)
	return c
}

// reply is one probe's answer. A probe cut short by the end of the run is left
// out, not counted as lost: its wait had not run out.
type reply struct {
	rtt time.Duration
	ok  bool
	cut bool
}

// prober sends probe seq and waits up to replyWait for its answer.
type prober interface {
	probe(ctx context.Context, seq int) reply
	close()
}

// outcome is what one method's probes saw. protocol is "" when no method got an
// answer; icmpErr says why ICMP could not be sent at all.
type outcome struct {
	protocol string
	port     int
	icmpErr  error
	replies  []reply
}

func answered(rs []reply) bool {
	for _, r := range rs {
		if r.ok {
			return true
		}
	}
	return false
}

// measure sends ICMP echoes, and if none is answered, opens TCP connections to the
// first port that answers; only probes from that first answer on are counted,
// since before it a lost probe and a filtered port cannot be told apart.
func measure(ctx context.Context, cfg Config, gw netip.Addr) outcome {
	var o outcome
	if p, err := cfg.icmp(gw); err != nil {
		o.icmpErr = err
	} else {
		rs := sample(ctx, p, rand.IntN(1<<16), probes, cfg.interval)
		p.close()
		if answered(rs) {
			return outcome{protocol: "icmp", replies: rs}
		}
	}
	for _, port := range tcpPorts {
		if ctx.Err() != nil {
			return o
		}
		p := cfg.tcp(gw, port)
		first := p.probe(ctx, 0)
		if first.ok {
			rs := append([]reply{first}, sample(ctx, p, 1, probes-1, cfg.interval)...)
			p.close()
			return outcome{protocol: "tcp", port: port, icmpErr: o.icmpErr, replies: rs}
		}
		p.close()
	}
	return o
}

// sample sends n probes numbered from first, one every interval, each waiting in
// its own goroutine, and stops sending when ctx ends.
func sample(ctx context.Context, p prober, first, n int, every time.Duration) []reply {
	rs := make([]reply, n)
	tick := time.NewTicker(every)
	defer tick.Stop()
	var wg sync.WaitGroup
	sent := 0
	for i := range n {
		if i > 0 {
			select {
			case <-ctx.Done():
			case <-tick.C:
			}
		}
		if ctx.Err() != nil {
			break
		}
		sent++
		wg.Go(func() { rs[i] = p.probe(ctx, first+i) })
	}
	wg.Wait()
	return rs[:sent]
}

type tcpProber struct {
	addr string
	dial func(ctx context.Context, network, address string) (net.Conn, error)
}

func newTCP(gw netip.Addr, port int) prober {
	return tcpProber{addr: net.JoinHostPort(gw.String(), strconv.Itoa(port)), dial: netx.Dialer(replyWait, 0).DialContext}
}

// probe times a handshake; a reset answers as well as a SYN-ACK does.
func (p tcpProber) probe(ctx context.Context, _ int) reply {
	start := time.Now()
	c, err := p.dial(ctx, "tcp", p.addr)
	rtt := time.Since(start)
	switch {
	case err == nil:
		c.Close()
		return reply{rtt: rtt, ok: true}
	case netx.IsRefused(err):
		return reply{rtt: rtt, ok: true}
	case ctx.Err() != nil:
		return reply{cut: true}
	}
	return reply{}
}

func (tcpProber) close() {}

// icmpProber sends echo requests on one socket; a reader goroutine hands each
// reply to the probe waiting for its sequence number.
type icmpProber struct {
	conn      *icmp.PacketConn
	gw        netip.Addr
	dst       net.Addr
	proto     int
	req, resp icmp.Type
	id        int

	mu      sync.Mutex
	waiting map[int]chan time.Time
	done    chan struct{}
}

func newICMP(gw netip.Addr) (prober, error) {
	family, proto, req, resp := "ipv4", 1, icmp.Type(ipv4.ICMPTypeEcho), icmp.Type(ipv4.ICMPTypeEchoReply)
	if !gw.Unmap().Is4() {
		family, proto, req, resp = "ipv6", 58, ipv6.ICMPTypeEchoRequest, ipv6.ICMPTypeEchoReply
	}
	conn, err := netx.ListenICMP(family)
	if err != nil {
		return nil, err
	}
	p := &icmpProber{
		conn: conn, gw: gw.Unmap().WithZone(""), proto: proto, req: req, resp: resp, id: rand.IntN(1 << 16),
		dst:     &net.UDPAddr{IP: gw.Unmap().AsSlice(), Zone: gw.Zone()},
		waiting: map[int]chan time.Time{}, done: make(chan struct{}),
	}
	go p.read()
	return p, nil
}

func (p *icmpProber) read() {
	defer close(p.done)
	b := make([]byte, 1500)
	for {
		n, from, err := p.conn.ReadFrom(b)
		at := time.Now()
		if err != nil {
			return
		}
		m, err := icmp.ParseMessage(p.proto, b[:n])
		if err != nil || m.Type != p.resp {
			continue
		}
		echo, ok := m.Body.(*icmp.Echo)
		if !ok || !p.from(from) || (runtime.GOOS != "linux" && echo.ID != p.id) {
			continue // Linux puts its own ID in, and lets only this socket's replies through
		}
		p.mu.Lock()
		ch := p.waiting[echo.Seq]
		delete(p.waiting, echo.Seq)
		p.mu.Unlock()
		if ch != nil {
			ch <- at
		}
	}
}

func (p *icmpProber) from(a net.Addr) bool {
	u, ok := a.(*net.UDPAddr)
	if !ok {
		return false
	}
	ip, ok := netip.AddrFromSlice(u.IP)
	return ok && ip.Unmap() == p.gw
}

func (p *icmpProber) probe(ctx context.Context, seq int) reply {
	seq &= 0xffff
	ch := make(chan time.Time, 1)
	p.mu.Lock()
	p.waiting[seq] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.waiting, seq)
		p.mu.Unlock()
	}()
	b, err := (&icmp.Message{Type: p.req, Body: &icmp.Echo{ID: p.id, Seq: seq, Data: make([]byte, echoBytes)}}).Marshal(nil)
	if err != nil {
		return reply{}
	}
	sent := time.Now()
	if _, err := p.conn.WriteTo(b, p.dst); err != nil {
		return reply{} // not sent is lost: the host could not reach its gateway
	}
	wait := time.NewTimer(replyWait)
	defer wait.Stop()
	select {
	case at := <-ch:
		return reply{rtt: at.Sub(sent), ok: true}
	case <-wait.C:
		return reply{}
	case <-ctx.Done():
		return reply{cut: true}
	}
}

func (p *icmpProber) close() {
	p.conn.Close()
	<-p.done
}
