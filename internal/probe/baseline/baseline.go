// Package baseline runs the udp-baseline and tcp-baseline checks side by side in
// one node session: round-trip time, its variation and, for UDP, loss, late,
// duplicate and reordered packets on an idle path.
package baseline

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/netx"
	"github.com/Lynthar/ConnVerifier/internal/probe/echo"
	"github.com/Lynthar/ConnVerifier/internal/probe/nodeclient"
	"github.com/Lynthar/ConnVerifier/internal/protocol"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

const (
	defaultTmax   = 3 * time.Second // RFC 6673 §3.2 waiting time
	sessionMargin = 10 * time.Second
	maxRate       = protocol.MaxStampRate / 2 // the session asks for twice the rate
	maxDuration   = time.Hour
	udpPacketLen  = protocol.StampLen
	tcpPacketLen  = 11 // a PING frame
)

type Config struct {
	node        string
	rate        int
	duration    time.Duration
	dialTimeout time.Duration
	tmax        time.Duration
	slipLimit   time.Duration
	// Tests replace the network; nil means netx.
	dial    nodeclient.DialFunc
	dialUDP func(ctx context.Context, address string) (net.Conn, error)
}

// RegisterFlags binds cfg to fs under the command-line names and defaults.
func (cfg *Config) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&cfg.node, "node", "", "invite of the node to test, or @FILE to read it from a file (default: $"+nodeclient.InviteEnv+")")
	fs.IntVar(&cfg.rate, "rate", 50, "probe packets per second, for each of UDP and TCP")
	fs.DurationVar(&cfg.duration, "duration", 20*time.Second, "how long each probe stream runs")
	fs.DurationVar(&cfg.dialTimeout, "dial-timeout", 5*time.Second, "timeout for reaching the node")
	cfg.tmax = defaultTmax
}

// Validate reports the first setting that is out of range.
func (cfg Config) Validate() error {
	if _, err := nodeclient.ResolveInvite(cfg.node); err != nil {
		return err
	}
	switch {
	case cfg.rate < 1 || cfg.rate > maxRate:
		return fmt.Errorf("rate must be 1 to %d", maxRate)
	case cfg.duration < time.Second || cfg.duration > maxDuration:
		return fmt.Errorf("duration must be 1s to %v", maxDuration)
	case cfg.dialTimeout <= 0:
		return errors.New("dial-timeout must be positive")
	}
	return nil
}

// Shared returns the settings the load check takes from these flags.
func (cfg Config) Shared() (node string, rate int, dialTimeout time.Duration) {
	return cfg.node, cfg.rate, cfg.dialTimeout
}

func (cfg Config) packets() int { return int(math.Round(float64(cfg.rate) * cfg.duration.Seconds())) }

func (cfg Config) want() protocol.Limits {
	return protocol.Limits{
		Connections:  1,
		DialRate:     1,
		DurationS:    int(math.Ceil((cfg.duration + 2*cfg.tmax + sessionMargin).Seconds())),
		IdleTimeoutS: 60,
		StampRate:    min(2*cfg.rate, protocol.MaxStampRate),
	}
}

func (cfg Config) params(packetLen int) map[string]any {
	return map[string]any{
		"rate_per_s":    cfg.rate,
		"duration_ms":   cfg.duration.Milliseconds(),
		"reply_wait_ms": cfg.tmax.Milliseconds(),
		"packet_bytes":  packetLen,
	}
}

// Run opens one session on the node, runs both checks in it at the same time and
// returns their results, UDP first. It returns an error only when cfg is invalid.
func Run(ctx context.Context, cfg Config, version string) ([]result.Check, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	if cfg.tmax <= 0 {
		cfg.tmax = defaultTmax
	}
	if cfg.slipLimit <= 0 {
		cfg.slipLimit = echo.DefaultSlipLimit
	}
	inv, _ := nodeclient.ResolveInvite(cfg.node)
	dial := cfg.dial
	if dial == nil {
		dial = netx.Dialer(cfg.dialTimeout, 0).DialContext
	}
	dialUDP := cfg.dialUDP
	if dialUDP == nil {
		dialUDP = func(ctx context.Context, a string) (net.Conn, error) { return netx.DialUDP(ctx, a) }
	}

	start := time.Now()
	udp := newCheck("udp-baseline", "udp", cfg, inv.Addrs[0], inv.Label, udpPacketLen)
	tcp := newCheck("tcp-baseline", "tcp", cfg, inv.Addrs[0], inv.Label, tcpPacketLen)
	done := func() []result.Check {
		el := time.Since(start).Milliseconds()
		udp.ElapsedMs, tcp.ElapsedMs = el, el
		return []result.Check{udp, tcp}
	}

	sess, err := nodeclient.Open(ctx, inv, dial, cfg.dialTimeout, version, protocol.SessionRequest{Check: protocol.CheckBaseline, Want: cfg.want()})
	if err != nil {
		var ne *nodeclient.Error
		if !errors.As(err, &ne) {
			return nil, err
		}
		nodeclient.Failed(&udp, ne)
		nodeclient.Failed(&tcp, ne)
		return done(), nil
	}
	for _, c := range []*result.Check{&udp, &tcp} {
		c.Path.Node, c.Path.Family = sess.Addr, nodeclient.Family(sess.Addr)
		g := sess.Resp.Granted
		c.Node.Version = sess.Resp.Node.Version
		c.Node.ObservedAddr = sess.Resp.ObservedAddr
		c.Node.Granted = &result.Grant{Connections: g.Connections, DialRate: g.DialRate, DurationS: g.DurationS, IdleTimeoutS: g.IdleTimeoutS, StampRate: g.StampRate}
		c.Node.LoadStart = nodeclient.Load(&sess.Resp.Node)
	}
	if time.Duration(sess.Resp.Granted.DurationS)*time.Second < cfg.duration+2*cfg.tmax {
		end := sess.End()
		for _, c := range []*result.Check{&udp, &tcp} {
			c.Node.LoadEnd = nodeclient.EndLoad(end)
			notRun(c, "baseline.warning.duration_not_granted", map[string]any{"granted_ms": sess.Resp.Granted.DurationS * 1000})
		}
		return done(), nil
	}

	var wg sync.WaitGroup
	var ur *echo.UDPRun
	var tr *echo.TCPRun
	var tfail *echo.DataFailure
	udpAddr, hasPort := inv.UDPAddr(sess.Addr)
	switch {
	case !hasPort:
		unsupported(&udp, "udp_baseline.unsupported.invite_without_udp")
	case sess.Resp.Stamp == nil:
		unsupported(&udp, "udp_baseline.unsupported.node_without_stamp")
	case sess.Resp.Granted.StampRate < cfg.rate:
		notRun(&udp, "baseline.warning.rate_not_granted", map[string]any{"granted": sess.Resp.Granted.StampRate, "asked": cfg.rate})
	default:
		conn, err := dialUDP(ctx, udpAddr)
		if err != nil {
			udp.Status = result.Error
			udp.Error = &result.Message{Key: "udp_baseline.error.no_socket", Params: map[string]any{"error": err.Error()}}
			break
		}
		wg.Go(func() {
			defer conn.Close()
			ur = &echo.UDPRun{}
			ur.Run(ctx, conn, sess.Secret, sess.Resp.Stamp.SSID, echo.Schedule(cfg.packets(), cfg.duration, newRand()), nil, cfg.tmax)
		})
	}
	wg.Go(func() {
		conn, fail := echo.OpenData(ctx, dial, sess.Addr, sess.ID, sess.Secret, cfg.dialTimeout)
		if fail != nil {
			tfail = fail
			return
		}
		defer conn.Close()
		tr = &echo.TCPRun{}
		tr.Run(ctx, conn, echo.Schedule(cfg.packets(), cfg.duration, newRand()), nil, cfg.tmax)
	})
	wg.Wait()
	end := sess.End() // after both streams stopped, so nothing is in flight
	udp.Node.LoadEnd, tcp.Node.LoadEnd = nodeclient.EndLoad(end), nodeclient.EndLoad(end)

	interrupted := ctx.Err() != nil
	if ur != nil {
		var counts *protocol.StampCounts
		if end != nil {
			counts = end.Stamp
		}
		finishUDP(&udp, ur, counts, cfg.tmax, cfg.slipLimit)
		if interrupted && metricValue(udp, "sent") == 0 {
			interruptedBeforeData(&udp)
		}
	}
	switch {
	case tfail != nil && interrupted:
		interruptedBeforeData(&tcp)
	case tfail != nil:
		tcpFailed(&tcp, tfail)
	case tr != nil:
		finishTCP(&tcp, tr, cfg.tmax, cfg.slipLimit)
	}
	return done(), nil
}

func newCheck(id, proto string, cfg Config, addr, label string, packetLen int) result.Check {
	return result.Check{
		ID:            id,
		MethodVersion: 1,
		Path:          result.Path{Node: addr, Family: nodeclient.Family(addr), Protocol: proto},
		Params:        cfg.params(packetLen),
		Metrics:       []result.Metric{},
		Node:          &result.NodeReport{Label: label},
	}
}

func newRand() *rand.Rand { return rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())) }
