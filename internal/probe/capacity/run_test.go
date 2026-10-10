package capacity

import (
	"context"
	"flag"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/memnet"
	"github.com/Lynthar/ConnVerifier/internal/node"
	"github.com/Lynthar/ConnVerifier/internal/probe/nodeclient"
	"github.com/Lynthar/ConnVerifier/internal/protocol"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

type memNode struct {
	invite string
	dial   nodeclient.DialFunc
	stop   func()
}

// startNode runs a real node in memory with one invite, limited by lim, and node
// flags such as -max-conns. Every dial the client makes becomes a pipe into it.
func startNode(t *testing.T, lim node.InviteLimits, flags ...string) memNode {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "node")
	inv, err := node.CreateInvite(dir, "mem", []string{"192.0.2.10:7443"}, 0, lim)
	if err != nil {
		t.Fatal(err)
	}
	var cfg node.Config
	fs := flag.NewFlagSet("node", flag.ContinueOnError)
	cfg.RegisterFlags(fs)
	if err := fs.Parse(append([]string{"-state-dir", dir}, flags...)); err != nil {
		t.Fatal(err)
	}
	ln := memnet.NewListener()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { node.ServeOn(ctx, ln, nil, nil, cfg, "vtest"); close(stopped) }()
	return memNode{invite: inv.Encode(), dial: ln.Dial, stop: func() { cancel(); <-stopped }}
}

func memRun(n memNode, clients int, duration time.Duration) Config {
	cfg := validClientConfig()
	cfg.node = n.invite
	cfg.targetConnections = clients
	cfg.startRate = 1000
	cfg.heartbeat = 30 * time.Millisecond
	cfg.minBackoff, cfg.maxBackoff = 10*time.Millisecond, 50*time.Millisecond
	cfg.duration = duration
	cfg.dial = n.dial
	return cfg
}

func metric(t *testing.T, c result.Check, id string) float64 {
	t.Helper()
	for _, m := range c.Metrics {
		if m.ID == id && m.Value != nil {
			return *m.Value
		}
	}
	t.Fatalf("metric %s missing", id)
	return 0
}

func messageKeys(msgs []result.Message) []string {
	var k []string
	for _, m := range msgs {
		k = append(k, m.Key)
	}
	return k
}

// waitGoroutines polls until the goroutine count is back to base; shutdown is
// asynchronous, so an immediate comparison would be flaky.
func waitGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, true)
			t.Fatalf("%d goroutines still running, baseline %d\n%s", runtime.NumGoroutine(), base, buf[:n])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunAgainstNode(t *testing.T) {
	base := runtime.NumGoroutine()
	n := startNode(t, node.DefaultInviteLimits)
	c, err := Run(context.Background(), memRun(n, 3, 300*time.Millisecond), "vtest")
	n.stop()
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != result.Pass {
		t.Fatalf("status = %s, want PASS; warnings %v error %+v", c.Status, messageKeys(c.Warnings), c.Error)
	}
	if got := metric(t, c, "connects"); got != 3 {
		t.Fatalf("connects = %v, want 3", got)
	}
	if got := metric(t, c, "drops"); got != 0 {
		t.Fatalf("drops = %v, want 0", got)
	}
	nr := c.Node
	if nr.Label != "mem" || nr.Version != "vtest" || nr.Granted == nil || nr.Granted.Connections != 3 {
		t.Fatalf("node report %+v", nr)
	}
	if nr.LoadEnd == nil || nr.LoadEnd.Sessions != 0 || nr.LoadEnd.Connections != 0 {
		t.Fatalf("session not released at the end: %+v", nr.LoadEnd)
	}
	if c.ElapsedMs < 300 {
		t.Fatalf("elapsed %d ms, want at least the run", c.ElapsedMs)
	}
	waitGoroutines(t, base)
}

// A node at its connection limit says so: the client counts refusals, not network
// drops, and the result is INVALID because the node, not the network, set the limit.
func TestFullNodeRejectionsAreNotDrops(t *testing.T) {
	n := startNode(t, node.DefaultInviteLimits, "-max-conns", "2")
	defer n.stop()
	c, err := Run(context.Background(), memRun(n, 5, 400*time.Millisecond), "vtest")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != result.Invalid || !slices.Contains(messageKeys(c.Warnings), "tcp_capacity.warning.node_busy") {
		t.Fatalf("status %s warnings %v, want INVALID with node_busy", c.Status, messageKeys(c.Warnings))
	}
	if got := metric(t, c, "rejected.busy"); got != 3 {
		t.Fatalf("rejected.busy = %v, want 3", got)
	}
	if got := metric(t, c, "drops.error") + metric(t, c, "drops.closed"); got != 0 {
		t.Fatalf("refusals counted as drops: %v", got)
	}
}

// A connection the node closes between heartbeats is recorded when the close
// arrives, so its lifetime is real, not rounded up to the next heartbeat.
func TestNodeCloseIsSeenWhenItArrives(t *testing.T) {
	base := runtime.NumGoroutine()
	n := startNode(t, node.DefaultInviteLimits)
	cfg := memRun(n, 2, 3*time.Second)
	cfg.heartbeat = 5 * time.Second
	cfg.minBackoff, cfg.maxBackoff = time.Second, time.Second
	time.AfterFunc(300*time.Millisecond, n.stop)
	c, err := Run(context.Background(), cfg, "vtest")
	if err != nil {
		t.Fatal(err)
	}
	if got := metric(t, c, "drops.node_closed"); got != 2 {
		t.Fatalf("drops.node_closed = %v, want 2", got)
	}
	if mean := metric(t, c, "dropped_session.mean"); mean > 1000 {
		t.Fatalf("mean lifetime %v ms with a 5s heartbeat: the close was not seen when it arrived", mean)
	}
	waitGoroutines(t, base)
}

func TestWrongPinStopsBeforeTheToken(t *testing.T) {
	n := startNode(t, node.DefaultInviteLimits)
	defer n.stop()
	inv, err := protocol.ParseInvite(n.invite)
	if err != nil {
		t.Fatal(err)
	}
	inv.Pin[0] ^= 0xff
	cfg := memRun(n, 1, 200*time.Millisecond)
	cfg.node = inv.Encode()
	c, err := Run(context.Background(), cfg, "vtest")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != result.Error || c.Error == nil || c.Error.Key != "session.error.node_identity" {
		t.Fatalf("status %s error %+v, want ERROR node_identity", c.Status, c.Error)
	}
	if c.Node.Granted != nil {
		t.Fatal("a session was granted despite the pin mismatch")
	}
}

func TestIdleGrantTooShortDoesNotRun(t *testing.T) {
	lim := node.DefaultInviteLimits
	lim.MaxIdleTimeoutS = 1
	n := startNode(t, lim)
	defer n.stop()
	cfg := memRun(n, 2, time.Second)
	cfg.heartbeat = 2 * time.Second
	c, err := Run(context.Background(), cfg, "vtest")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != result.Invalid || !slices.Contains(messageKeys(c.Warnings), "tcp_capacity.warning.idle_too_short") {
		t.Fatalf("status %s warnings %v, want INVALID idle_too_short", c.Status, messageKeys(c.Warnings))
	}
	if len(c.Metrics) != 0 {
		t.Fatalf("metrics from a run that should not have started: %+v", c.Metrics)
	}
	if c.Node.LoadEnd == nil || c.Node.LoadEnd.Sessions != 0 {
		t.Fatalf("session not released: %+v", c.Node.LoadEnd)
	}
}

func TestGrantSmallerThanAskedIsInvalid(t *testing.T) {
	lim := node.DefaultInviteLimits
	lim.MaxConnections = 2
	n := startNode(t, lim)
	defer n.stop()
	c, err := Run(context.Background(), memRun(n, 5, 300*time.Millisecond), "vtest")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != result.Invalid || !slices.Contains(messageKeys(c.Warnings), "tcp_capacity.warning.granted_less") {
		t.Fatalf("status %s warnings %v, want INVALID granted_less", c.Status, messageKeys(c.Warnings))
	}
	if got := metric(t, c, "connects"); got != 2 {
		t.Fatalf("connects = %v, want the granted 2", got)
	}
}

func TestSessionRefusedWhenInviteIsBusy(t *testing.T) {
	lim := node.DefaultInviteLimits
	lim.MaxSessions = 1
	n := startNode(t, lim)
	defer n.stop()
	first := make(chan result.Check, 1)
	go func() {
		c, _ := Run(context.Background(), memRun(n, 1, 600*time.Millisecond), "vtest")
		first <- c
	}()
	time.Sleep(200 * time.Millisecond)
	c, err := Run(context.Background(), memRun(n, 1, 200*time.Millisecond), "vtest")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != result.Error || c.Error == nil || c.Error.Key != "session.error.node_busy" {
		t.Fatalf("status %s error %+v, want ERROR node_busy", c.Status, c.Error)
	}
	if f := <-first; f.Status != result.Pass {
		t.Fatalf("first run = %s", f.Status)
	}
}

func TestRunCancelledReturnsResult(t *testing.T) {
	n := startNode(t, node.DefaultInviteLimits)
	defer n.stop()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	c, err := Run(ctx, memRun(n, 2, 0), "vtest")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status == result.Error {
		t.Fatalf("cancellation reported as ERROR: %+v", c.Error)
	}
}
