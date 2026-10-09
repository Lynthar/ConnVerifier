package load

import (
	"context"
	"flag"
	"net"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/memnet"
	"github.com/Lynthar/ConnVerifier/internal/node"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

// memNode is a real node in memory: TLS, HTTP/2, the load endpoints, the data
// plane and the STAMP reflector, with no sockets.
type memNode struct {
	invite string
	ln     *memnet.Listener
	pc     *memnet.PacketConn
	stop   func()
}

func startNode(t *testing.T, lim node.InviteLimits, flags ...string) memNode {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "node")
	inv, err := node.CreateInvite(dir, "mem", []string{"192.0.2.10:7443"}, 7443, lim)
	if err != nil {
		t.Fatal(err)
	}
	var cfg node.Config
	fs := flag.NewFlagSet("node", flag.ContinueOnError)
	cfg.RegisterFlags(fs)
	fs.Parse(append([]string{"-state-dir", dir}, flags...))
	n := memNode{invite: inv.Encode(), ln: memnet.NewListener(), pc: memnet.NewPacketConn("192.0.2.10:7443")}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { node.ServeOn(ctx, n.ln, n.pc, cfg, "vtest"); close(stopped) }()
	n.stop = func() { cancel(); <-stopped }
	t.Cleanup(n.stop)
	return n
}

// memConfig is a short run: 100 ms intervals, phases of at most 2 s and mb MB.
// Validity limits are off: in memory the load is bound by this machine's CPU
// by construction, which is exactly what they would report.
func memConfig(n memNode, mb int64) Config {
	return Config{
		interval: 100 * time.Millisecond, phaseTime: 2 * time.Second, phaseMB: mb,
		node: n.invite, rate: 50, dialTimeout: 2 * time.Second,
		slipLimit: time.Second, cpuMax: 2,
		dial:    n.ln.Dial,
		dialUDP: func(context.Context, string) (net.Conn, error) { return n.pc.Dial(), nil },
	}
}

func metric(c result.Check, id string) (result.Metric, bool) {
	i := slices.IndexFunc(c.Metrics, func(m result.Metric) bool { return m.ID == id })
	if i < 0 {
		return result.Metric{}, false
	}
	return c.Metrics[i], true
}

func value(t *testing.T, c result.Check, id string) float64 {
	t.Helper()
	m, ok := metric(c, id)
	if !ok || m.Value == nil {
		t.Fatalf("metric %s missing or without a value: %+v", id, m)
	}
	return *m.Value
}

func keys(c result.Check) []string {
	var k []string
	for _, g := range [][]result.Message{c.Inferences, c.NotProven, c.Warnings} {
		for _, m := range g {
			k = append(k, m.Key)
		}
	}
	if c.Error != nil {
		k = append(k, c.Error.Key)
	}
	return k
}

func waitGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("%d goroutines, %d before\n%s", runtime.NumGoroutine(), base, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Every phase runs against a real node and stops at its traffic limit; what the
// client counted and what the node counted agree, and nothing is left running.
func TestPhasesStopAtTheTrafficLimit(t *testing.T) {
	base := runtime.NumGoroutine()
	n := startNode(t, node.DefaultInviteLimits)
	c, err := Run(context.Background(), memConfig(n, 2), "vtest", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("status %s %v", c.Status, keys(c))
	if c.Status == result.Error || c.Status == result.Invalid || c.Status == result.Unsupported {
		t.Fatalf("status %s %v", c.Status, keys(c))
	}
	if c.ElapsedMs < 1000 {
		t.Errorf("elapsed %d ms for three phases", c.ElapsedMs)
	}
	var client float64
	for _, p := range []string{"download", "upload", "bidirectional"} {
		b := value(t, c, p+".bytes")
		if b < 2e6 || b > 3e6 {
			t.Errorf("%s moved %v bytes; want the 2 MB limit plus at most a chunk per connection", p, b)
		}
		client += b
	}
	for _, id := range []string{"download.goodput", "upload.goodput", "bidirectional.goodput.download", "bidirectional.goodput.upload"} {
		if m, _ := metric(c, id); m.Value == nil || *m.Value <= 0 {
			t.Errorf("%s = %+v, want a positive goodput", id, m)
		}
	}
	tr := c.Node.Traffic
	if tr == nil || tr.LagTicks == 0 {
		t.Fatalf("node traffic %+v", tr)
	}
	// Uploads are counted as handed to the transport, so a little may be in flight.
	if node := float64(tr.SentBytes + tr.ReceivedBytes); node > client || node < client-4e6 {
		t.Errorf("node counted %v bytes, client %v", node, client)
	}
	n.stop()
	waitGoroutines(t, base)
}

func TestNodeWithoutLoad(t *testing.T) {
	n := startNode(t, node.DefaultInviteLimits, "-max-load-sessions", "0")
	c, _ := Run(context.Background(), memConfig(n, 2), "vtest", nil)
	if c.Status != result.Unsupported || !slices.Contains(keys(c), "load.unsupported.node_without_load") {
		t.Fatalf("status %s %v", c.Status, keys(c))
	}
}

func TestInterruptedBeforeTheFirstPhase(t *testing.T) {
	n := startNode(t, node.DefaultInviteLimits)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, _ := Run(ctx, memConfig(n, 2), "vtest", nil)
	if c.Status != result.Invalid || !slices.Contains(keys(c), "load.warning.interrupted") {
		t.Fatalf("status %s %v; an interrupt is not a node failure", c.Status, keys(c))
	}
}

// link is one direction of a bottleneck shared by every connection: a token
// bucket of rate bytes per second holding at most 10 ms of them.
type link struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
}

func newLink(rate float64) *link { return &link{rate: rate, last: time.Now()} }

func (l *link) wait(n int) {
	for {
		l.mu.Lock()
		now := time.Now()
		l.tokens = min(l.rate/100, l.tokens+now.Sub(l.last).Seconds()*l.rate)
		l.last = now
		if l.tokens >= float64(n) || l.tokens >= l.rate/100 {
			l.tokens -= float64(n)
			l.mu.Unlock()
			return
		}
		need := time.Duration((float64(n) - l.tokens) / l.rate * float64(time.Second))
		l.mu.Unlock()
		time.Sleep(need)
	}
}

// throttled passes a connection's bytes through the two directions of a link.
type throttled struct {
	net.Conn
	down, up *link
}

func (c *throttled) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p[:min(len(p), 4096)])
	c.down.wait(n)
	return n, err
}

func (c *throttled) Write(p []byte) (int, error) {
	var done int
	for len(p) > 0 {
		k := min(len(p), 4096)
		c.up.wait(k)
		n, err := c.Conn.Write(p[:k])
		done += n
		if err != nil {
			return done, err
		}
		p = p[k:]
	}
	return done, nil
}

// Behind a 4 MB/s bottleneck each way, goodput settles at the bottleneck less
// framing and the probes' share, and the result says so with high confidence.
// The ceiling is tight: counting above the socket overstated uploads by 6%.
func TestGoodputSettlesAtTheBottleneck(t *testing.T) {
	if testing.Short() {
		t.Skip("runs for about twenty seconds")
	}
	const rate = 4e6
	n := startNode(t, node.DefaultInviteLimits)
	down, up := newLink(rate), newLink(rate)
	cfg := memConfig(n, 100)
	// The node reports upload progress every 50 ms per stream: an interval needs
	// several reports, or the counts per interval jump by whole reports.
	cfg.interval, cfg.phaseTime = 250*time.Millisecond, 5*time.Second
	cfg.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := n.ln.Dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &throttled{Conn: c, down: down, up: up}, nil
	}
	c, err := Run(context.Background(), cfg, "vtest", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("status %s %v", c.Status, keys(c))
	for _, id := range []string{"download.goodput", "upload.goodput"} {
		m, _ := metric(c, id)
		conf := value(t, c, strings.TrimSuffix(id, "goodput")+"goodput.confidence")
		t.Logf("%s %v Mbit/s at least %v, confidence %v", id, *m.Value, m.AtLeast, conf)
		if got := *m.Value; got < 0.9*rate*8/1e6 || got > 1.02*rate*8/1e6 {
			t.Errorf("%s = %v Mbit/s; want the 32 Mbit/s bottleneck less framing and probes", id, got)
		}
		if conf != result.High || m.AtLeast {
			t.Errorf("%s: confidence %v, at least %v; want settled", id, conf, m.AtLeast)
		}
	}
}
