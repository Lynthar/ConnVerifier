//go:build unix

package capacity

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/netx"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatalf("list open descriptors: %v", err)
	}
	return len(entries)
}

// The only capacity test on real sockets: Run against a loopback echo listener
// must leave descriptors and goroutines where they were before it started.
func TestRunReleasesSocketsAndGoroutines(t *testing.T) {
	if testing.Short() {
		t.Skip("opens loopback sockets")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := netx.Listen(ctx, "127.0.0.1:0", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go echo(conn)
		}
	}()

	baseFDs, baseGoroutines := openFDs(t), runtime.NumGoroutine()
	cfg := shortRun(nil)
	cfg.address = ln.Addr().String()
	c, err := Run(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != result.Pass {
		t.Fatalf("status = %s, want PASS; metrics %+v", c.Status, c.Metrics)
	}

	waitGoroutines(t, baseGoroutines)
	deadline := time.Now().Add(2 * time.Second)
	for openFDs(t) > baseFDs {
		if time.Now().After(deadline) {
			t.Fatalf("%d descriptors open after Run, %d before", openFDs(t), baseFDs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
