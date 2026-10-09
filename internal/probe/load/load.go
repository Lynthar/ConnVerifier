// Package load runs the tcp-load check: it loads the path to a node until goodput
// and responsiveness settle (draft-ietf-ippm-responsiveness-09 §5) in three
// phases — download, upload, both — while the baseline's STAMP and echo streams
// keep running beside the load as independent probes.
package load

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/netenv"
	"github.com/Lynthar/ConnVerifier/internal/netx"
	"github.com/Lynthar/ConnVerifier/internal/probe/echo"
	"github.com/Lynthar/ConnVerifier/internal/probe/nodeclient"
	"github.com/Lynthar/ConnVerifier/internal/protocol"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

const (
	defaultInterval  = time.Second
	defaultPhaseTime = 20 * time.Second
	defaultPhaseMB   = 500
	// Between phases the check waits until the independent probes' round trips are
	// back near idle — the last phase's queue has drained — at least minDrain and at
	// most maxDrain.
	minDrain   = 2 * time.Second
	maxDrain   = 10 * time.Second
	drainEvery = 250 * time.Millisecond
	// connectTimeout bounds opening a load connection or a foreign probe: TCP and
	// TLS handshakes cross the loaded queue, seconds deep on a bloated uplink.
	connectTimeout = 15 * time.Second
	tmax           = 3 * time.Second // independent probes' waiting time, as in the baseline
	sessionMargin  = 10 * time.Second
	probeAllowance = 10_000_000 // bytes the session asks for beyond the phases, for probes
	lagPeriod      = 10 * time.Millisecond
)

type Config struct {
	interval  time.Duration
	phaseTime time.Duration
	phaseMB   int64
	// Shared with the baseline flags; see Use.
	node        string
	rate        int
	dialTimeout time.Duration
	// Tests replace these; zero means the method's value and netx.
	slipLimit time.Duration
	cpuMax    float64
	dial      nodeclient.DialFunc
	dialUDP   func(ctx context.Context, address string) (net.Conn, error)
}

// RegisterFlags binds the load flags to fs; -node, -rate and -dial-timeout belong
// to the baseline and reach this check through Use.
func (cfg *Config) RegisterFlags(fs *flag.FlagSet) {
	fs.DurationVar(&cfg.interval, "load-interval", defaultInterval, "interval at which the load check adds a connection and tests for a settled value (the draft's ID)")
	fs.DurationVar(&cfg.phaseTime, "load-time", defaultPhaseTime, "longest each of the three load phases runs")
	fs.Int64Var(&cfg.phaseMB, "load-mb", defaultPhaseMB, "most traffic each load phase moves, in MB; above the default the run needs confirming")
}

// Use takes the settings shared with the baseline checks.
func (cfg *Config) Use(node string, rate int, dialTimeout time.Duration) {
	cfg.node, cfg.rate, cfg.dialTimeout = node, rate, dialTimeout
}

// Validate reports the first setting that is out of range.
func (cfg Config) Validate() error {
	switch {
	case cfg.interval < 100*time.Millisecond || cfg.interval > 10*time.Second:
		return errors.New("load-interval must be 100ms to 10s")
	case cfg.phaseTime < cfg.interval || cfg.phaseTime > 10*time.Minute:
		return errors.New("load-time must be from load-interval to 10m")
	case cfg.phaseMB < 1 || cfg.phaseMB > 100_000:
		return errors.New("load-mb must be 1 to 100000")
	}
	return nil
}

// MaxMB is the most traffic the load phases may move, announced before the run.
func (cfg Config) MaxMB() int64 { return int64(len(phases)) * cfg.phaseMB }

// Large reports whether the run moves more than the default allows; such a run
// starts only after the user agrees.
func (cfg Config) Large() bool { return cfg.phaseMB > defaultPhaseMB }

// Notice says how much traffic the load phases may move.
func (cfg Config) Notice() result.Message {
	return result.Message{Key: "load.notice", Params: map[string]any{"mb": cfg.MaxMB(), "phase_mb": cfg.phaseMB, "phases": len(phases)}}
}

func (cfg Config) stage() time.Duration {
	return time.Duration(len(phases))*cfg.phaseTime + time.Duration(len(phases)-1)*maxDrain
}

func (cfg Config) want() protocol.Limits {
	return protocol.Limits{
		Connections:     1,
		DialRate:        1,
		DurationS:       int(math.Ceil((cfg.stage() + 2*tmax + sessionMargin).Seconds())),
		IdleTimeoutS:    60,
		StampRate:       min(2*cfg.rate, protocol.MaxStampRate),
		LoadBytes:       int64(len(phases))*cfg.phaseMB*1e6 + probeAllowance,
		LoadConnections: 2*mnp + probeHeadroom,
	}
}

func (cfg Config) params() map[string]any {
	return map[string]any{
		"method":             "draft-ietf-ippm-responsiveness-09",
		"interval_ms":        cfg.interval.Milliseconds(),
		"phase_time_ms":      cfg.phaseTime.Milliseconds(),
		"phase_mb":           cfg.phaseMB,
		"max_connections":    mnp,
		"probe_pairs_per_s":  mps,
		"probe_rate_per_s":   cfg.rate,
		"moving_average":     mad,
		"tolerance_percent":  sdt * 100,
		"trimmed_mean_keeps": trimKeep * 100,
	}
}

// Run opens a load session on the node, runs the three phases and returns the
// tcp-load result. baseline holds the idle checks run just before, whose round
// trips the loaded ones are compared with. It returns an error only when cfg is
// invalid.
func Run(ctx context.Context, cfg Config, version string, baseline []result.Check) (c result.Check, err error) {
	if err := cfg.Validate(); err != nil {
		return result.Check{}, fmt.Errorf("invalid configuration: %w", err)
	}
	inv, err := nodeclient.ResolveInvite(cfg.node)
	if err != nil {
		return result.Check{}, fmt.Errorf("invalid configuration: %w", err)
	}
	if cfg.slipLimit <= 0 {
		cfg.slipLimit = echo.DefaultSlipLimit
	}
	if cfg.cpuMax <= 0 {
		cfg.cpuMax = 1 - 0.5/float64(runtime.GOMAXPROCS(0)) // half a core short of all of them
	}
	dial := cfg.dial
	if dial == nil {
		dial = netx.Dialer(cfg.dialTimeout, 0).DialContext
	}
	dialUDP := cfg.dialUDP
	if dialUDP == nil {
		dialUDP = func(ctx context.Context, a string) (net.Conn, error) { return netx.DialUDP(ctx, a) }
	}

	start := time.Now()
	c = result.Check{
		ID:            "tcp-load",
		MethodVersion: 1,
		Path:          result.Path{Node: inv.Addrs[0], Family: nodeclient.Family(inv.Addrs[0]), Protocol: "tcp"},
		Params:        cfg.params(),
		Metrics:       []result.Metric{},
		Node:          &result.NodeReport{Label: inv.Label},
	}
	defer func() { c.ElapsedMs = time.Since(start).Milliseconds() }()

	sess, err := nodeclient.Open(ctx, inv, dial, cfg.dialTimeout, version, protocol.CheckLoad, cfg.want())
	if err != nil {
		var ne *nodeclient.Error
		switch {
		case ctx.Err() != nil: // stopped by the user, not refused by the node
			c.Status = result.Invalid
			c.Warnings = append(c.Warnings, result.Message{Key: "load.warning.interrupted"})
		case errors.As(err, &ne):
			nodeclient.Failed(&c, ne)
		default:
			return result.Check{}, err
		}
		return c, nil
	}
	g, lg := sess.Resp.Granted, sess.Resp.Load
	c.Path.Node, c.Path.Family = sess.Addr, nodeclient.Family(sess.Addr)
	c.Node.Version, c.Node.ObservedAddr = sess.Resp.Node.Version, sess.Resp.ObservedAddr
	c.Node.Granted = &result.Grant{Connections: g.Connections, DialRate: g.DialRate, DurationS: g.DurationS, IdleTimeoutS: g.IdleTimeoutS, StampRate: g.StampRate}
	if lg != nil && lg.Refused == "" {
		c.Node.Granted.LoadBytes, c.Node.Granted.LoadConnections = lg.Bytes, lg.Connections
	}
	c.Node.LoadStart = nodeclient.Load(&sess.Resp.Node)
	switch {
	case lg == nil:
		c.Node.LoadEnd = nodeclient.EndLoad(sess.End())
		c.Status = result.Unsupported
		c.Warnings = append(c.Warnings, result.Message{Key: "load.unsupported.node_without_load"})
		return c, nil
	case lg.Refused != "":
		c.Node.LoadEnd = nodeclient.EndLoad(sess.End())
		c.Status = result.Error
		c.Error = &result.Message{Key: "load.error.refused_" + lg.Refused, Params: map[string]any{"retry_after_ms": lg.RetryAfterS * 1000}}
		return c, nil
	case time.Duration(g.DurationS)*time.Second < cfg.stage()+2*tmax:
		c.Node.LoadEnd = nodeclient.EndLoad(sess.End())
		c.Status = result.Invalid
		c.Warnings = append(c.Warnings, result.Message{Key: "load.warning.duration_not_granted", Params: map[string]any{"granted_ms": g.DurationS * 1000}})
		return c, nil
	}

	loadDial := cfg.dial
	if loadDial == nil {
		loadDial = netx.Dialer(connectTimeout, 0).DialContext
	}
	t := &target{addr: sess.Addr, token: protocol.EncodeID(inv.Token[:]), session: sess.Resp.SessionID, pin: inv.Pin, dial: loadDial, timeout: connectTimeout}
	allowance := min(probeAllowance, lg.Bytes/10) // a small grant still leaves the phases most of it
	phaseBytes := min(cfg.phaseMB*1e6, (lg.Bytes-allowance)/int64(len(phases)))
	maxConns := max(lg.Connections-probeHeadroom, 2)
	probes := startProbes(ctx, cfg, sess, inv, dial, dialUDP)
	base := idleOf(baseline)
	lag := netenv.StartLag(lagPeriod, cfg.slipLimit)
	var results []phaseResult
	for i, spec := range phases {
		var waited time.Duration
		drained := true
		if i > 0 {
			waited, drained = probes.drain(ctx, base)
		}
		if ctx.Err() != nil {
			results = append(results, phaseResult{spec: spec, stopped: stopInterrupted})
			continue
		}
		ticks0, over0 := lag.Counts()
		cpu0, cpuOK := netenv.ProcessCPU()
		r := runPhase(ctx, t, spec, cfg.interval, cfg.phaseTime, phaseBytes, maxConns)
		r.waited, r.drained = waited, drained
		ticks1, over1 := lag.Counts()
		r.lagTicks, r.lagOver = ticks1-ticks0, over1-over0
		if cpu1, ok := netenv.ProcessCPU(); ok && cpuOK {
			r.cpuKnown = true
			r.cpuBusy = (cpu1 - cpu0).Seconds() / (r.end.Sub(r.start).Seconds() * float64(runtime.GOMAXPROCS(0)))
		}
		results = append(results, r)
	}
	lag.Stop()
	ur, tr := probes.finish()
	end := sess.End() // after the load and the probes stopped, so the counts are final
	c.Node.LoadEnd = nodeclient.EndLoad(end)
	var counts *protocol.LoadCounts
	if end != nil {
		counts = end.Load
		if counts != nil {
			c.Node.Traffic = &result.Traffic{SentBytes: counts.Sent, ReceivedBytes: counts.Received, LagTicks: counts.LagTicks, LagOver: counts.LagOver}
		}
	}
	finish(&c, results, ur, tr, probes.notes, counts, base, cfg)
	return c, nil
}

// probes are the baseline's two streams, run across the whole load stage on one
// socket each: the node latches a session's first UDP source address, and one
// sequence space keeps a late reply from one phase off the next one's probes.
type probes struct {
	stop   chan struct{}
	wg     sync.WaitGroup
	udp    *echo.UDPRun // nil when it cannot run
	tcp    *echo.TCPRun
	tcpRan bool // set before wg.Done
	start  time.Time
	notes  []result.Message
}

func startProbes(ctx context.Context, cfg Config, sess *nodeclient.Session, inv protocol.Invite, dial nodeclient.DialFunc, dialUDP func(context.Context, string) (net.Conn, error)) *probes {
	p := &probes{stop: make(chan struct{}), tcp: &echo.TCPRun{}, start: time.Now()}
	span := cfg.stage() + time.Second // stop closes it at the stage's real end
	n := int(math.Round(float64(cfg.rate) * span.Seconds()))
	rng := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	udpAddr, hasPort := inv.UDPAddr(sess.Addr)
	switch {
	case !hasPort || sess.Resp.Stamp == nil || sess.Resp.Granted.StampRate < cfg.rate:
		p.notes = append(p.notes, result.Message{Key: "load.not_proven.no_udp_probe"})
	default:
		conn, err := dialUDP(ctx, udpAddr)
		if err != nil {
			p.notes = append(p.notes, result.Message{Key: "load.not_proven.no_udp_probe"})
			break
		}
		offsets := echo.Schedule(n, span, rng)
		p.udp = &echo.UDPRun{}
		p.wg.Go(func() {
			defer conn.Close()
			p.udp.Run(ctx, conn, sess.Secret, sess.Resp.Stamp.SSID, offsets, p.stop, tmax)
		})
	}
	offsets := echo.Schedule(n, span, rng)
	p.wg.Go(func() {
		conn, fail := echo.OpenData(ctx, dial, sess.Addr, sess.ID, sess.Secret, cfg.dialTimeout)
		if fail != nil {
			err := fail.Err
			if err == nil {
				err = errors.New(fail.Reject.Reason.String())
			}
			p.notes = append(p.notes, result.Message{Key: "load.not_proven.no_tcp_probe", Params: map[string]any{"error": err.Error()}})
			return
		}
		defer conn.Close()
		p.tcp.Run(ctx, conn, offsets, p.stop, tmax)
		p.tcpRan = true
	})
	return p
}

// drain waits until the probes' round trips are back near idle, or maxDrain; it
// returns how long it waited and whether they came back. Without a probe stream
// it waits minDrain.
func (p *probes) drain(ctx context.Context, base idle) (time.Duration, bool) {
	start := time.Now()
	s, ref := &p.tcp.Stream, base.tcpP50
	if p.udp != nil {
		s, ref = &p.udp.Stream, base.udpP50
	}
	t := time.NewTicker(drainEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return time.Since(start), false
		case <-t.C:
		}
		waited := time.Since(start)
		if waited < minDrain {
			continue
		}
		reference := time.Duration(ref * float64(time.Millisecond))
		if reference <= 0 { // no valid baseline: the least this stream has seen
			reference = slices.Min(append(s.RTTs(p.start), time.Hour))
		}
		recent := s.RTTs(time.Now().Add(-time.Second))
		if drainedTo(recent, reference) {
			return waited, true
		}
		if waited >= maxDrain || (len(recent) == 0 && len(s.RTTs(p.start)) == 0) {
			return waited, len(recent) == 0 && len(s.RTTs(p.start)) == 0
		}
	}
}

// drainedTo reports whether the median of recent round trips is within 10 ms or
// 20% of ref, whichever is more; it needs three of them.
func drainedTo(recent []time.Duration, ref time.Duration) bool {
	if len(recent) < 3 {
		return false
	}
	s := slices.Clone(recent)
	slices.Sort(s)
	return s[len(s)/2] <= ref+max(10*time.Millisecond, ref/5)
}

// finish stops sending, lets the last probes' waiting time pass and returns both
// streams; either is nil when it never ran.
func (p *probes) finish() (*echo.UDPRun, *echo.TCPRun) {
	close(p.stop)
	p.wg.Wait()
	if !p.tcpRan {
		return p.udp, nil
	}
	return p.udp, p.tcp
}

// idle holds the baseline's round-trip quantiles, in ms, from checks that
// measured validly; a zero value is unknown.
type idle struct {
	udpP50, udpP95, tcpP50, tcpP95 float64
}

func idleOf(checks []result.Check) idle {
	var i idle
	for _, c := range checks {
		if c.Status != result.Pass && c.Status != result.Warn {
			continue
		}
		get := func(id string) float64 {
			for _, m := range c.Metrics {
				if m.ID == id && m.Value != nil {
					return *m.Value
				}
			}
			return 0
		}
		switch c.ID {
		case "udp-baseline":
			i.udpP50, i.udpP95 = get("rtt.p50"), get("rtt.p95")
		case "tcp-baseline":
			i.tcpP50, i.tcpP95 = get("rtt.p50"), get("rtt.p95")
		}
	}
	return i
}
