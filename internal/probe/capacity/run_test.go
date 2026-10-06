package capacity

import (
	"context"
	"io"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/result"
)

// pipeNode stands in for a node without opening sockets: each dial gets one end
// of a net.Pipe and the other end runs serveConn.
func pipeNode(serveConn func(net.Conn)) func(context.Context, string, string) (net.Conn, error) {
	return func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go serveConn(server)
		return client, nil
	}
}

func echo(conn net.Conn) {
	defer conn.Close()
	io.Copy(conn, conn)
}

// closeAfterFirst answers one heartbeat and then closes, like a node that turns
// every connection away right after accepting it.
func closeAfterFirst(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, len(pingPayload))
	if _, err := io.ReadFull(conn, buf); err == nil {
		conn.Write(buf)
	}
}

func shortRun(dial func(context.Context, string, string) (net.Conn, error)) Config {
	cfg := validClientConfig()
	cfg.targetConnections = 3
	cfg.startRate = 1000
	cfg.heartbeat = 20 * time.Millisecond
	cfg.minBackoff = 10 * time.Millisecond
	cfg.maxBackoff = 50 * time.Millisecond
	cfg.duration = 200 * time.Millisecond
	cfg.dial = dial
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

// waitGoroutines polls until the goroutine count is back to base; shutdown is
// asynchronous, so an immediate comparison would be flaky.
func waitGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, true)
			t.Fatalf("%d goroutines still running, baseline %d\n%s", runtime.NumGoroutine(), base, buf[:n])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunAgainstEchoNode(t *testing.T) {
	base := runtime.NumGoroutine()
	c, err := Run(context.Background(), shortRun(pipeNode(echo)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != result.Pass {
		t.Fatalf("status = %s, want PASS; metrics %+v", c.Status, c.Metrics)
	}
	if got := metric(t, c, "connects"); got != 3 {
		t.Fatalf("connects = %v, want 3", got)
	}
	if got := metric(t, c, "drops"); got != 0 {
		t.Fatalf("drops = %v, want 0", got)
	}
	waitGoroutines(t, base)
}

func TestRunAgainstClosingNode(t *testing.T) {
	base := runtime.NumGoroutine()
	c, err := Run(context.Background(), shortRun(pipeNode(closeAfterFirst)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != result.Warn {
		t.Fatalf("status = %s, want WARN", c.Status)
	}
	if got := metric(t, c, "drops"); got == 0 {
		t.Fatal("no drops recorded against a node that closes every connection")
	}
	if got := metric(t, c, "heartbeats.interval_acked"); got != 0 {
		t.Fatalf("interval_acked = %v, want 0", got)
	}
	waitGoroutines(t, base)
}

func TestRunCancelledReturnsResult(t *testing.T) {
	base := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	cfg := shortRun(pipeNode(echo))
	cfg.duration = 0
	time.AfterFunc(100*time.Millisecond, cancel)
	c, err := Run(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status == result.Error {
		t.Fatalf("cancellation reported as ERROR")
	}
	waitGoroutines(t, base)
}
