//go:build unix

package capacity

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/netx"
	"github.com/Lynthar/ConnVerifier/internal/node"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

func openFDs(t *testing.T) int {
	t.Helper()
	dir, err := os.Open("/dev/fd")
	if err != nil {
		t.Fatalf("list open descriptors: %v", err)
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1) // names only: a stat races descriptors closing elsewhere
	if err != nil {
		t.Fatalf("list open descriptors: %v", err)
	}
	return len(names)
}

// The only capacity test on real sockets: client and node over loopback, TLS
// control plane and data plane through the shared port, and afterwards every
// descriptor and goroutine back where it was.
func TestLoopbackRunReleasesEverything(t *testing.T) {
	if testing.Short() {
		t.Skip("opens loopback sockets")
	}
	baseFDs, baseGoroutines := openFDs(t), runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	ln, err := netx.Listen(ctx, "127.0.0.1:0", 0)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "node")
	inv, err := node.CreateInvite(dir, "loopback", []string{ln.Addr().String()}, node.DefaultInviteLimits)
	if err != nil {
		t.Fatal(err)
	}
	var ncfg node.Config
	fs := flag.NewFlagSet("node", flag.ContinueOnError)
	ncfg.RegisterFlags(fs)
	fs.Parse([]string{"-state-dir", dir})
	stopped := make(chan struct{})
	go func() { node.ServeOn(ctx, ln, ncfg, "vtest"); close(stopped) }()

	cfg := validClientConfig()
	cfg.node = inv.Encode()
	cfg.targetConnections, cfg.startRate = 3, 1000
	cfg.heartbeat = 30 * time.Millisecond
	cfg.duration = 300 * time.Millisecond
	c, err := Run(context.Background(), cfg, "vtest")
	cancel()
	<-stopped
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != result.Pass || c.Path.Family != "ipv4" {
		t.Fatalf("status %s family %q; warnings %v error %+v", c.Status, c.Path.Family, keys(c.Warnings), c.Error)
	}

	waitGoroutines(t, baseGoroutines)
	deadline := time.Now().Add(2 * time.Second)
	for openFDs(t) > baseFDs {
		if time.Now().After(deadline) {
			t.Fatalf("%d descriptors open after the run, %d before", openFDs(t), baseFDs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
