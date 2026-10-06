// Package node is the echo endpoint that clients hold connections against: it
// reflects every byte and drops peers that stay idle past the configured timeout.
package node

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/netx"
)

type Config struct {
	address        string
	maxConns       int
	idleTimeout    time.Duration
	tcpKeepAlive   time.Duration
	logConnections bool
}

type Stats struct {
	accepted uint64
	rejected uint64
	closed   uint64
	active   int64
}

// RegisterFlags binds cfg to fs under the command-line names and defaults.
func (cfg *Config) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&cfg.address, "addr", ":9000", "TCP listen address")
	fs.IntVar(&cfg.maxConns, "max-conns", 10000, "maximum concurrent connections; set <=0 for unlimited")
	fs.DurationVar(&cfg.idleTimeout, "idle-timeout", 2*time.Minute, "idle timeout per connection; set <=0 to disable")
	fs.DurationVar(&cfg.tcpKeepAlive, "tcp-keepalive", 0, "TCP keepalive probe interval on accepted connections; <=0 disables (keepalive refreshes NAT mappings and masks the idle timeout under test)")
	fs.BoolVar(&cfg.logConnections, "log-connections", false, "log every connection open and close")
}

// Serve accepts and echoes connections until ctx ends. It returns an error when
// cfg is invalid or the listener cannot be opened; otherwise nil after shutdown.
func Serve(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	listener, err := netx.Listen(ctx, cfg.address, cfg.tcpKeepAlive)
	if err != nil {
		return fmt.Errorf("listen failed: %w", err)
	}
	serve(ctx, listener, cfg)
	return nil
}

// serve runs the accept loop on listener until ctx ends, then closes it.
func serve(ctx context.Context, listener net.Listener, cfg Config) {
	log.Printf("echo server listening on %s max_conns=%d idle_timeout=%s tcp_keepalive=%s",
		listener.Addr(), cfg.maxConns, cfg.idleTimeout, cfg.tcpKeepAlive)

	stats := &Stats{}
	go reportStats(ctx, stats)

	// Unblock Accept on shutdown so the loop can exit cleanly.
	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	limit := makeConnectionLimit(cfg.maxConns)

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Printf("accept failed: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		if !acquireConnection(limit) {
			atomic.AddUint64(&stats.rejected, 1)
			if cfg.logConnections {
				log.Printf("connection rejected from %s active=%d rejected=%d",
					conn.RemoteAddr(), atomic.LoadInt64(&stats.active), atomic.LoadUint64(&stats.rejected))
			}
			conn.Close()
			continue
		}

		atomic.AddUint64(&stats.accepted, 1)
		atomic.AddInt64(&stats.active, 1)
		go handle(conn, cfg, func() {
			releaseConnection(limit)
			atomic.AddInt64(&stats.active, -1)
			atomic.AddUint64(&stats.closed, 1)
		})
	}

	log.Printf("shutdown: stopped accepting; active=%d accepted=%d rejected=%d closed=%d",
		atomic.LoadInt64(&stats.active), atomic.LoadUint64(&stats.accepted),
		atomic.LoadUint64(&stats.rejected), atomic.LoadUint64(&stats.closed))
}

func reportStats(ctx context.Context, stats *Stats) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			log.Printf("stats active=%d accepted=%d rejected=%d closed=%d",
				atomic.LoadInt64(&stats.active),
				atomic.LoadUint64(&stats.accepted),
				atomic.LoadUint64(&stats.rejected),
				atomic.LoadUint64(&stats.closed),
			)
		}
	}
}

// Validate reports the first setting that is out of range.
func (cfg Config) Validate() error {
	if cfg.address == "" {
		return fmt.Errorf("addr must not be empty")
	}
	return nil
}

func makeConnectionLimit(maxConns int) chan struct{} {
	if maxConns <= 0 {
		return nil
	}
	return make(chan struct{}, maxConns)
}

func acquireConnection(limit chan struct{}) bool {
	if limit == nil {
		return true
	}
	select {
	case limit <- struct{}{}:
		return true
	default:
		return false
	}
}

func releaseConnection(limit chan struct{}) {
	if limit == nil {
		return
	}
	<-limit
}

func handle(conn net.Conn, cfg Config, done func()) {
	defer conn.Close()
	if done != nil {
		defer done()
	}
	if cfg.logConnections {
		log.Printf("connection opened from %s", conn.RemoteAddr())
		defer log.Printf("connection closed from %s", conn.RemoteAddr())
	}

	buf := make([]byte, 4096)
	for {
		if cfg.idleTimeout > 0 {
			if err := conn.SetDeadline(time.Now().Add(cfg.idleTimeout)); err != nil {
				log.Printf("set deadline failed for %s: %v", conn.RemoteAddr(), err)
				return
			}
		}

		n, err := conn.Read(buf)
		if n > 0 {
			if writeErr := netx.WriteFull(conn, buf[:n]); writeErr != nil {
				if cfg.logConnections {
					log.Printf("echo write error to %s: %v", conn.RemoteAddr(), writeErr)
				}
				return
			}
		}
		if err != nil {
			if cfg.logConnections {
				if err == io.EOF {
					log.Printf("remote closed %s", conn.RemoteAddr())
				} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					log.Printf("idle timeout from %s", conn.RemoteAddr())
				} else {
					log.Printf("read error from %s: %v", conn.RemoteAddr(), err)
				}
			}
			return
		}
	}
}
