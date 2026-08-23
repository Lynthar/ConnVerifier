package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

type ServerConfig struct {
	address        string
	maxConns       int
	idleTimeout    time.Duration
	tcpKeepAlive   time.Duration
	logConnections bool
}

type ServerStats struct {
	accepted uint64
	rejected uint64
	closed   uint64
	active   int64
}

func main() {
	addr := flag.String("addr", ":9000", "TCP listen address")
	maxConns := flag.Int("max-conns", 10000, "maximum concurrent connections; set <=0 for unlimited")
	idleTimeout := flag.Duration("idle-timeout", 2*time.Minute, "idle timeout per connection; set <=0 to disable")
	tcpKeepAlive := flag.Duration("tcp-keepalive", 0, "TCP keepalive probe interval on accepted connections; <=0 disables (keepalive refreshes NAT mappings and masks the idle timeout under test)")
	logConnections := flag.Bool("log-connections", false, "log every connection open and close")
	flag.Parse()

	cfg := ServerConfig{
		address:        *addr,
		maxConns:       *maxConns,
		idleTimeout:    *idleTimeout,
		tcpKeepAlive:   *tcpKeepAlive,
		logConnections: *logConnections,
	}
	if err := validateServerConfig(cfg); err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// KeepAlive: -1 disables keepalive at accept time. Go's default (zero) would
	// silently enable 15s probes, refreshing the very NAT mappings the client is
	// trying to age out; configureTCPKeepAlive re-enables it when the flag is >0.
	lc := net.ListenConfig{KeepAlive: -1}
	listener, err := lc.Listen(ctx, "tcp", cfg.address)
	if err != nil {
		log.Fatalf("listen failed: %v", err)
	}
	log.Printf("echo server listening on %s max_conns=%d idle_timeout=%s tcp_keepalive=%s",
		listener.Addr(), cfg.maxConns, cfg.idleTimeout, cfg.tcpKeepAlive)

	stats := &ServerStats{}
	go reportServerStats(ctx, stats)

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

		configureTCPKeepAlive(conn, cfg.tcpKeepAlive)
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

func reportServerStats(ctx context.Context, stats *ServerStats) {
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

func validateServerConfig(cfg ServerConfig) error {
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

func configureTCPKeepAlive(conn net.Conn, interval time.Duration) {
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok || interval <= 0 {
		return
	}
	if err := tcpConn.SetKeepAlive(true); err != nil {
		log.Printf("set tcp keepalive failed for %s: %v", conn.RemoteAddr(), err)
		return
	}
	if err := tcpConn.SetKeepAlivePeriod(interval); err != nil {
		log.Printf("set tcp keepalive period failed for %s: %v", conn.RemoteAddr(), err)
	}
}

func handle(conn net.Conn, cfg ServerConfig, done func()) {
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
			if writeErr := writeFull(conn, buf[:n]); writeErr != nil {
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

func writeFull(conn net.Conn, data []byte) error {
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
