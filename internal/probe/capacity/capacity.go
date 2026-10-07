// Package capacity holds a fixed pool of long-lived TCP connections against a node,
// heartbeating each one and recording how and when they drop. It opens a session
// on the node first, and every connection it makes counts against that session.
package capacity

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/netx"
	"github.com/Lynthar/ConnVerifier/internal/probe/nodeclient"
	"github.com/Lynthar/ConnVerifier/internal/protocol"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

const (
	maxStartRate  = 100000
	maxClients    = 1000000
	limiterTick   = 10 * time.Millisecond
	dialLogWindow = 10 * time.Second
	// sessionMargin is how long before the session's end the run stops, so the
	// node's CLOSE at expiry never lands on a connection still being measured.
	sessionMargin = 5 * time.Second
	minIdleAsk    = 60 * time.Second
)

// fdReserve is the descriptors a run needs besides its connections: stdio, the
// runtime's poller and the like.
const fdReserve = 32

type Config struct {
	node              string
	targetConnections int
	startRate         int
	heartbeat         time.Duration
	dialTimeout       time.Duration
	ioTimeout         time.Duration
	minBackoff        time.Duration
	maxBackoff        time.Duration
	duration          time.Duration
	logDrops          bool
	dial              nodeclient.DialFunc // nil: netx
}

// RegisterFlags binds cfg to fs under the command-line names and defaults.
func (cfg *Config) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&cfg.node, "node", "", "invite of the node to test, or @FILE to read it from a file (default: $"+nodeclient.InviteEnv+")")
	fs.IntVar(&cfg.targetConnections, "clients", 1000, "target number of concurrent connections")
	fs.IntVar(&cfg.startRate, "start-rate", 100, "max new connections started per second")
	fs.DurationVar(&cfg.heartbeat, "heartbeat", 30*time.Second, "heartbeat interval")
	fs.DurationVar(&cfg.dialTimeout, "dial-timeout", 5*time.Second, "TCP dial timeout")
	fs.DurationVar(&cfg.ioTimeout, "io-timeout", 5*time.Second, "deadline for the node's answer to a ticket or heartbeat")
	fs.DurationVar(&cfg.minBackoff, "min-backoff", 500*time.Millisecond, "initial reconnect backoff")
	fs.DurationVar(&cfg.maxBackoff, "max-backoff", time.Minute, "maximum reconnect backoff")
	fs.DurationVar(&cfg.duration, "duration", 0, "stop after this long; <=0 runs until interrupted or the node's session ends")
	fs.BoolVar(&cfg.logDrops, "log-drops", false, "log every connection drop with its reason")
}

// invite resolves -node: an invite string, @FILE, or the environment variable.
func (cfg Config) invite() (protocol.Invite, error) { return nodeclient.ResolveInvite(cfg.node) }

// Validate reports the first setting that is out of range.
func (cfg Config) Validate() error {
	if _, err := cfg.invite(); err != nil {
		return err
	}
	switch {
	case cfg.targetConnections <= 0 || cfg.targetConnections > maxClients:
		return fmt.Errorf("clients must be 1 to %d", maxClients)
	case cfg.startRate <= 0 || cfg.startRate > maxStartRate:
		return fmt.Errorf("start-rate must be 1 to %d", maxStartRate)
	case cfg.heartbeat <= 0:
		return errors.New("heartbeat must be positive")
	case cfg.dialTimeout <= 0:
		return errors.New("dial-timeout must be positive")
	case cfg.ioTimeout <= 0:
		return errors.New("io-timeout must be positive")
	case cfg.minBackoff <= 0:
		return errors.New("min-backoff must be positive")
	case cfg.maxBackoff < cfg.minBackoff:
		return errors.New("max-backoff must be >= min-backoff")
	case cfg.heartbeat+cfg.ioTimeout > time.Duration(protocol.MaxIdleTimeoutS)*time.Second:
		return fmt.Errorf("heartbeat plus io-timeout must stay under %ds, the longest idle period a node can grant", protocol.MaxIdleTimeoutS)
	}
	return nil
}

// Notice describes what the run will do to the network, for the confirmation the
// user must give before a stress check starts. cfg must have passed Validate.
func (cfg Config) Notice() result.Message {
	inv, _ := cfg.invite()
	key := "tcp_capacity.confirm.notice"
	if cfg.duration <= 0 {
		key = "tcp_capacity.confirm.notice_until_stopped"
	}
	return result.Message{Key: key, Params: map[string]any{
		"node":        inv.Label,
		"addr":        inv.Addrs[0],
		"connections": cfg.targetConnections,
		"rate":        cfg.startRate,
		"duration_ms": cfg.duration.Milliseconds(),
	}}
}

// want is what the run asks the node for. The idle period covers a heartbeat plus
// its reply with room to spare; the duration leaves room for the stop margin.
func (cfg Config) want() protocol.Limits {
	w := protocol.Limits{
		Connections:  cfg.targetConnections,
		DialRate:     cfg.startRate,
		IdleTimeoutS: ceilSeconds(max(2*(cfg.heartbeat+cfg.ioTimeout), minIdleAsk)),
	}
	w.IdleTimeoutS = min(w.IdleTimeoutS, protocol.MaxIdleTimeoutS)
	if cfg.duration > 0 {
		w.DurationS = min(ceilSeconds(cfg.duration+2*sessionMargin), protocol.MaxDurationS)
	}
	return w
}

func ceilSeconds(d time.Duration) int { return int(math.Ceil(d.Seconds())) }

// Run asks the node for a session, holds the connection pool until ctx ends, the
// configured duration elapses or the session is about to end, and returns the
// check's result. It returns an error only when cfg is invalid.
func Run(ctx context.Context, cfg Config, version string) (c result.Check, err error) {
	if err := cfg.Validate(); err != nil {
		return result.Check{}, fmt.Errorf("invalid configuration: %w", err)
	}
	inv, _ := cfg.invite()
	dial := cfg.dial
	if dial == nil {
		dial = netx.Dialer(cfg.dialTimeout, 0).DialContext
	}
	start := time.Now()
	c = newCheck(cfg, inv.Addrs[0], inv.Label)
	defer func() { c.ElapsedMs = time.Since(start).Milliseconds() }()

	sess, err := nodeclient.Open(ctx, inv, dial, cfg.dialTimeout, version, protocol.CheckTCPCapacity, cfg.want())
	if err != nil {
		var se *nodeclient.Error
		if errors.As(err, &se) {
			nodeclient.Failed(&c, se)
			return c, nil
		}
		return result.Check{}, err
	}
	g := sess.Resp.Granted
	c.Path = newCheck(cfg, sess.Addr, inv.Label).Path
	c.Node.Version = sess.Resp.Node.Version
	c.Node.ObservedAddr = sess.Resp.ObservedAddr
	c.Node.Granted = &result.Grant{Connections: g.Connections, DialRate: g.DialRate, DurationS: g.DurationS, IdleTimeoutS: g.IdleTimeoutS}
	c.Node.LoadStart = nodeclient.Load(&sess.Resp.Node)

	if need := cfg.heartbeat + cfg.ioTimeout; time.Duration(g.IdleTimeoutS)*time.Second < need {
		c.Node.LoadEnd = nodeclient.EndLoad(sess.End())
		idleTooShort(&c, g, need)
		return c, nil
	}

	runCtx, cancel, nodeDeadline := runWindow(ctx, cfg.duration, start, sess.Created, g)
	stats := &Stats{rtt: newRttHistogram(), host: readHostLimits()}
	if c.Path.Family != "" {
		stats.family.Store(&c.Path.Family)
	}
	p := &pool{
		cfg:       cfg,
		addr:      sess.Addr,
		sessionID: sess.ID,
		secret:    sess.Secret,
		target:    min(cfg.targetConnections, g.Connections),
		rate:      min(cfg.startRate, g.DialRate),
		dial:      dial,
		stats:     stats,
	}
	f := p.run(runCtx)
	nodeCut := nodeDeadline && errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	cancel()
	c.Node.LoadEnd = nodeclient.EndLoad(sess.End()) // after every slot has closed its connection
	if c.Path.Family == "" {
		c.Path.Family = f.snap.family
	}
	finishCheck(&c, cfg, f, stats.rtt, runInfo{granted: g, nodeCut: nodeCut})
	return c, nil
}

// newCheck is the result before anything is known about the node beyond its invite.
func newCheck(cfg Config, addr, label string) result.Check {
	return result.Check{
		ID:            "tcp-capacity",
		MethodVersion: 3,
		Path:          result.Path{Node: addr, Family: nodeclient.Family(addr), Protocol: "tcp"},
		Params:        cfg.params(),
		Metrics:       []result.Metric{},
		Node:          &result.NodeReport{Label: label},
	}
}

// runWindow ends the run at -duration or shortly before the session's granted
// length, whichever comes first, and reports whether the session's length won.
// It counts from the client's own clock, so clock skew with the node cannot matter.
func runWindow(ctx context.Context, duration time.Duration, start, created time.Time, g protocol.Limits) (context.Context, context.CancelFunc, bool) {
	granted := time.Duration(g.DurationS) * time.Second
	stopAt := created.Add(granted - min(sessionMargin, granted/5))
	if duration > 0 && start.Add(duration).Before(stopAt) {
		ctx, cancel := context.WithDeadline(ctx, start.Add(duration))
		return ctx, cancel, false
	}
	ctx, cancel := context.WithDeadline(ctx, stopAt)
	return ctx, cancel, true
}

// params records the settings the user asked for, with "until interrupted" as 0.
func (cfg Config) params() map[string]any {
	return map[string]any{
		"target_connections": cfg.targetConnections,
		"start_rate":         cfg.startRate,
		"heartbeat_ms":       cfg.heartbeat.Milliseconds(),
		"dial_timeout_ms":    cfg.dialTimeout.Milliseconds(),
		"io_timeout_ms":      cfg.ioTimeout.Milliseconds(),
		"min_backoff_ms":     cfg.minBackoff.Milliseconds(),
		"max_backoff_ms":     cfg.maxBackoff.Milliseconds(),
		"duration_ms":        max(cfg.duration, 0).Milliseconds(),
	}
}
