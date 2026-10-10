package baseline

import (
	"context"
	"encoding/binary"
	"flag"
	"net"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/memnet"
	"github.com/Lynthar/ConnVerifier/internal/node"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

// memNode is a real node in memory: TLS, HTTP/2, the data plane and the STAMP
// reflector, with no sockets.
type memNode struct {
	invite string
	ln     *memnet.Listener
	pc     *memnet.PacketConn // nil: the node runs no reflector
	stop   func()
}

func startNode(t *testing.T, udpPort int, reflector bool) memNode {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "node")
	lim := node.DefaultInviteLimits
	lim.MaxStampRate = 1000
	inv, err := node.CreateInvite(dir, "mem", []string{"192.0.2.10:7443"}, udpPort, lim)
	if err != nil {
		t.Fatal(err)
	}
	var cfg node.Config
	fs := flag.NewFlagSet("node", flag.ContinueOnError)
	cfg.RegisterFlags(fs)
	fs.Parse([]string{"-state-dir", dir})
	n := memNode{invite: inv.Encode(), ln: memnet.NewListener()}
	var pc net.PacketConn
	if reflector {
		n.pc = memnet.NewPacketConn("192.0.2.10:7443")
		pc = n.pc
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { node.ServeOn(ctx, n.ln, pc, nil, cfg, "vtest"); close(stopped) }()
	n.stop = func() { cancel(); <-stopped }
	t.Cleanup(n.stop)
	return n
}

// memRun is a short run against n; forward filters what this host sends.
func memRun(n memNode, forward memnet.Filter) Config {
	return Config{
		node: n.invite, rate: 200, duration: time.Second, dialTimeout: time.Second,
		tmax: 300 * time.Millisecond, slipLimit: time.Second,
		dial: n.ln.Dial,
		dialUDP: func(context.Context, string) (net.Conn, error) {
			c := n.pc.Dial()
			c.Out = forward
			return c, nil
		},
	}
}

func run(t *testing.T, cfg Config) (udp, tcp result.Check) {
	t.Helper()
	checks, err := Run(context.Background(), cfg, "vtest")
	if err != nil || len(checks) != 2 {
		t.Fatalf("Run: %v (%d checks)", err, len(checks))
	}
	return checks[0], checks[1]
}

func metric(t *testing.T, c result.Check, id string) float64 {
	t.Helper()
	for _, m := range c.Metrics {
		if m.ID == id && m.Value != nil {
			return *m.Value
		}
	}
	t.Fatalf("%s: metric %s missing", c.ID, id)
	return 0
}

func keys(msgs []result.Message) []string {
	var k []string
	for _, m := range msgs {
		k = append(k, m.Key)
	}
	return k
}

func senderSeq(b []byte) uint32    { return binary.BigEndian.Uint32(b[0:]) }
func reflectedSeq(b []byte) uint32 { return binary.BigEndian.Uint32(b[48:]) }

func waitGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines, %d before", runtime.NumGoroutine(), base)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCleanPathPasses(t *testing.T) {
	base := runtime.NumGoroutine()
	n := startNode(t, 7443, true)
	udp, tcp := run(t, memRun(n, nil))
	if udp.Status != result.Pass || tcp.Status != result.Pass {
		t.Fatalf("udp %s %v %v, tcp %s %v", udp.Status, keys(udp.Warnings), udp.Error, tcp.Status, tcp.Error)
	}
	for _, id := range []string{"sent", "replies"} {
		if metric(t, udp, id) != 200 || metric(t, tcp, id) != 200 {
			t.Fatalf("%s: udp %v tcp %v, want 200", id, metric(t, udp, id), metric(t, tcp, id))
		}
	}
	if metric(t, udp, "lost.forward") != 0 || metric(t, udp, "lost.return") != 0 {
		t.Fatal("loss on a clean path")
	}
	if l := udp.Node.LoadEnd; l == nil || l.Sessions != 0 {
		t.Fatalf("session not released: %+v", l)
	}
	n.stop()
	waitGoroutines(t, base)
}

// Loss is split by direction from the node's own count: packets dropped on the
// way there never reached it, packets dropped on the way back did.
func TestLossIsSplitByDirection(t *testing.T) {
	n := startNode(t, 7443, true)
	n.pc.Out = func(b []byte) [][]byte {
		if reflectedSeq(b)%10 == 7 {
			return nil
		}
		return [][]byte{b}
	}
	udp, tcp := run(t, memRun(n, func(b []byte) [][]byte {
		if senderSeq(b)%10 == 3 {
			return nil
		}
		return [][]byte{b}
	}))
	if udp.Status != result.Warn || !slices.Contains(keys(udp.Inferences), "udp_baseline.inference.loss") {
		t.Fatalf("status %s inferences %v", udp.Status, keys(udp.Inferences))
	}
	if f, r, l := metric(t, udp, "lost.forward"), metric(t, udp, "lost.return"), metric(t, udp, "lost"); f != 20 || r != 20 || l != 40 {
		t.Fatalf("forward %v return %v total %v; want 20 20 40", f, r, l)
	}
	if v := metric(t, udp, "loss.forward"); v != 10 {
		t.Fatalf("forward loss %v%%, want 10", v)
	}
	if tcp.Status != result.Pass {
		t.Fatalf("tcp %s", tcp.Status)
	}
}

// A duplicate the node answered twice was duplicated on the way there; a reply
// arriving twice was duplicated on the way back.
func TestDuplicatesAreAttributedByDirection(t *testing.T) {
	n := startNode(t, 7443, true)
	n.pc.Out = func(b []byte) [][]byte {
		if reflectedSeq(b)%20 == 11 {
			return [][]byte{b, b}
		}
		return [][]byte{b}
	}
	udp, _ := run(t, memRun(n, func(b []byte) [][]byte {
		if senderSeq(b)%20 == 5 {
			return [][]byte{b, b}
		}
		return [][]byte{b}
	}))
	if f, r := metric(t, udp, "duplicates.forward"), metric(t, udp, "duplicates.return"); f != 10 || r != 10 {
		t.Fatalf("forward %v return %v; want 10 and 10", f, r)
	}
	if metric(t, udp, "lost.forward") != 0 || metric(t, udp, "lost.return") != 0 {
		t.Fatal("duplicates the node answered were read as loss")
	}
	if udp.Status != result.Warn || !slices.Contains(keys(udp.Inferences), "udp_baseline.inference.duplicates") {
		t.Fatalf("status %s inferences %v", udp.Status, keys(udp.Inferences))
	}
}

func TestNoUDPReachingTheNodeIsAnError(t *testing.T) {
	n := startNode(t, 7443, true)
	udp, tcp := run(t, memRun(n, func([]byte) [][]byte { return nil }))
	if udp.Status != result.Error || udp.Error == nil || udp.Error.Key != "udp_baseline.error.no_udp_to_node" {
		t.Fatalf("status %s error %+v", udp.Status, udp.Error)
	}
	if tcp.Status != result.Pass {
		t.Fatalf("tcp %s", tcp.Status)
	}
}

func TestBlockedReturnPathIsAWarning(t *testing.T) {
	n := startNode(t, 7443, true)
	n.pc.Out = func([]byte) [][]byte { return nil }
	udp, _ := run(t, memRun(n, nil))
	if udp.Status != result.Warn || !slices.Contains(keys(udp.Inferences), "udp_baseline.inference.return_blocked") {
		t.Fatalf("status %s inferences %v error %+v", udp.Status, keys(udp.Inferences), udp.Error)
	}
}

func TestUDPUnsupportedWithoutPortOrReflector(t *testing.T) {
	for _, tt := range []struct {
		name      string
		port      int
		reflector bool
		key       string
	}{
		{"invite without UDP port", 0, true, "udp_baseline.unsupported.invite_without_udp"},
		{"node without reflector", 7443, false, "udp_baseline.unsupported.node_without_stamp"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n := startNode(t, tt.port, tt.reflector)
			cfg := memRun(n, nil)
			cfg.dialUDP = nil // must not be reached
			udp, tcp := run(t, cfg)
			if udp.Status != result.Unsupported || !slices.Contains(keys(udp.Warnings), tt.key) {
				t.Fatalf("udp %s %v", udp.Status, keys(udp.Warnings))
			}
			if tcp.Status != result.Pass {
				t.Fatalf("tcp %s", tcp.Status)
			}
		})
	}
}

// Probes still inside their waiting time when a run is interrupted are left out,
// not counted as lost: stopping a run must not invent loss.
func TestInterruptedRunInventsNoLoss(t *testing.T) {
	base := runtime.NumGoroutine()
	n := startNode(t, 7443, true)
	cfg := memRun(n, nil)
	cfg.duration, cfg.tmax = 3*time.Second, time.Second
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(700*time.Millisecond, cancel)
	checks, err := Run(ctx, cfg, "vtest")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range checks {
		if c.Status == result.Error || metric(t, c, "sent") == 0 || metric(t, c, "sent") != metric(t, c, "replies") {
			t.Fatalf("%s: %s, sent %v replies %v", c.ID, c.Status, metric(t, c, "sent"), metric(t, c, "replies"))
		}
	}
	if checks[0].ElapsedMs > 2000 {
		t.Fatalf("took %d ms after an interrupt at 700 ms", checks[0].ElapsedMs)
	}
	n.stop()
	waitGoroutines(t, base)
}

// Stopping a run before it measured anything is not a failure of the network or
// the node: the checks are INVALID, not ERROR.
func TestInterruptBeforeDataIsNotAnError(t *testing.T) {
	n := startNode(t, 7443, true)
	cfg := memRun(n, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cfg.dialUDP = func(context.Context, string) (net.Conn, error) {
		cancel() // the user presses Ctrl-C as the streams start
		c := n.pc.Dial()
		return c, nil
	}
	checks, err := Run(ctx, cfg, "vtest")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range checks {
		if c.Status == result.Error {
			t.Fatalf("%s: ERROR %+v after an interrupt", c.ID, c.Error)
		}
	}
}
