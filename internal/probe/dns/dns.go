// Package dns runs the dns check: whether each resolver this host is configured
// to ask answers in time, and how long it takes, over UDP straight to the resolver.
// It needs no node.
package dns

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/netenv"
	"github.com/Lynthar/ConnVerifier/internal/netx"
	"github.com/Lynthar/ConnVerifier/internal/protocol"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

// ID is the check's ID.
const ID = "dns"

// MethodVersion goes up with any change to the query, the schedule, a definition,
// the estimators or the status rules.
const MethodVersion = 1

const (
	queries  = 20
	interval = 100 * time.Millisecond // 10 a second
	// replyWait bounds each query. Queries go out on schedule without waiting, so a
	// resolver that never answers adds this once, not once per query.
	replyWait = 2 * time.Second
	dnsPort   = 53
)

// Config holds what tests replace; the zero value asks this host's resolvers.
type Config struct {
	resolvers func() ([]netip.Addr, error)
	route     func(ctx context.Context, hostPort string) (netenv.Route, error)
	port      int
	interval  time.Duration
	wait      time.Duration
}

func (cfg Config) withDefaults() Config {
	if cfg.resolvers == nil {
		cfg.resolvers = netenv.Resolvers
	}
	if cfg.route == nil {
		cfg.route = netenv.RouteTo
	}
	if cfg.port == 0 {
		cfg.port = dnsPort
	}
	if cfg.interval <= 0 {
		cfg.interval = interval
	}
	if cfg.wait <= 0 {
		cfg.wait = replyWait
	}
	return cfg
}

func familyOf(a netip.Addr) string {
	if a.Unmap().Is4() {
		return "ipv4"
	}
	return "ipv6"
}

// Run asks every resolver in turn, IPv4 ones first: one check each, numbered by
// its place in the host's list. A family with none is one SKIP check.
func Run(ctx context.Context, cfg Config) []result.Check {
	cfg = cfg.withDefaults()
	list, err := cfg.resolvers()
	var checks []result.Check
	for _, family := range []string{"ipv4", "ipv6"} {
		if err != nil {
			c := newCheck(family)
			c.Status = result.Unsupported
			c.Warnings = append(c.Warnings, msg("dns.unsupported.no_config", map[string]any{"error": err.Error()}))
			checks = append(checks, c)
			continue
		}
		n := 0
		for i, r := range list {
			if familyOf(r) == family {
				n++
				checks = append(checks, ask(ctx, cfg, r, i+1))
			}
		}
		if n == 0 {
			c := newCheck(family)
			c.Status = result.Skip
			c.Warnings = append(c.Warnings, msg("dns.skip.no_resolver", map[string]any{"family": family}))
			checks = append(checks, c)
		}
	}
	return checks
}

func newCheck(family string) result.Check {
	return result.Check{
		ID: ID, MethodVersion: MethodVersion,
		Path: result.Path{Family: family, Protocol: "udp"},
		Params: map[string]any{
			"queries":       queries,
			"rate_per_s":    int(time.Second / interval),
			"reply_wait_ms": replyWait.Milliseconds(),
		},
		Metrics: []result.Metric{},
	}
}

func ask(ctx context.Context, cfg Config, r netip.Addr, index int) (c result.Check) {
	start := time.Now()
	c = newCheck(familyOf(r))
	defer func() { c.ElapsedMs = time.Since(start).Milliseconds() }()
	c.Params["resolver"] = index
	c.Params["resolver_class"] = netenv.AddrClass(r)
	addr := net.JoinHostPort(r.String(), strconv.Itoa(cfg.port))
	if rt, err := cfg.route(ctx, addr); err == nil && rt.Kind == netenv.KindTunnel {
		c.NotProven = append(c.NotProven, msg("dns.not_proven.tunnel", nil))
	}
	var o outcome
	conn, err := netx.DialUDP(ctx, addr)
	switch {
	case err != nil && ctx.Err() != nil: // the run ended first: nothing was sent
	case err != nil:
		o = outcome{answers: make([]answer, queries), sendErr: err}
		for i := range o.answers {
			o.answers[i].sent = true
		}
	default:
		o = exchange(ctx, conn, queries, cfg.interval, cfg.wait)
		conn.Close()
	}
	finish(&c, o, ctx.Err() != nil)
	return c
}

// answer is what became of one query. A query still within its wait when the run
// ended is cut: left out, not lost.
type answer struct {
	sent  bool
	ok    bool
	rtt   time.Duration
	rcode int
	cut   bool
}

// outcome is one resolver's answers; sendErr is the first error sending a query.
type outcome struct {
	answers []answer
	sendErr error
}

// exchange sends n queries one every interval on conn, a socket connected to the
// resolver, and takes each answer that comes within wait of its query; it stops
// listening once all n are answered.
func exchange(ctx context.Context, conn *net.UDPConn, n int, every, wait time.Duration) outcome {
	o := outcome{answers: make([]answer, n)}
	ids := make([]uint16, 0, n)
	index := make(map[uint16]int, n)
	for len(ids) < n {
		id := uint16(rand.Uint32())
		if _, dup := index[id]; !dup {
			index[id] = len(ids)
			ids = append(ids, id)
		}
	}
	sentAt := make([]time.Time, n)
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		b := make([]byte, 1500)
		for answered := 0; answered < n; {
			k, err := conn.Read(b)
			at := time.Now()
			if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, net.ErrClosed) {
				return
			}
			if err != nil {
				continue // a "port unreachable" for an earlier query, reported on this socket
			}
			id, rcode, err := protocol.ParseRootNSAnswer(b[:k])
			i, known := index[id]
			if err != nil || !known {
				continue
			}
			mu.Lock()
			if a := &o.answers[i]; a.sent && !a.ok && at.Sub(sentAt[i]) <= wait {
				a.ok, a.rtt, a.rcode = true, at.Sub(sentAt[i]), rcode
				answered++
			}
			mu.Unlock()
		}
	}()

	tick := time.NewTicker(every)
	defer tick.Stop()
	var last time.Time
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
		mu.Lock()
		sentAt[i], o.answers[i].sent = time.Now(), true
		last = sentAt[i]
		mu.Unlock()
		if _, err := conn.Write(protocol.RootNSQuery(ids[i])); err != nil && o.sendErr == nil {
			o.sendErr = err
		}
	}
	conn.SetReadDeadline(last.Add(wait))
	stop := context.AfterFunc(ctx, func() { conn.SetReadDeadline(time.Now()) })
	defer stop()
	if ctx.Err() != nil {
		conn.SetReadDeadline(time.Now())
	}
	<-done
	end := time.Now()
	for i := range o.answers {
		if a := &o.answers[i]; a.sent && !a.ok && end.Sub(sentAt[i]) < wait {
			a.cut = true
		}
	}
	return o
}
