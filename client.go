package main

import (
	"flag"
	"io"
	"log"
	"net"
	"sync/atomic"
	"time"
)

const pingPayload = "PING"

type RateLimiter struct {
	tokens chan struct{}
}

func newRateLimiter(rate int) *RateLimiter {
	if rate <= 0 {
		return nil
	}
	interval := time.Second / time.Duration(rate)
	if interval < time.Millisecond {
		interval = time.Millisecond
	}

	rl := &RateLimiter{tokens: make(chan struct{}, rate)}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			select {
			case rl.tokens <- struct{}{}:
			default:
			}
		}
	}()

	return rl
}

func (rl *RateLimiter) Wait() {
	if rl == nil {
		return
	}
	<-rl.tokens
}

type Slot struct {
	id      int
	backoff time.Duration
}

type Config struct {
	address           string
	targetConnections int
	startRate         int
	dialLimiter       *RateLimiter
	heartbeat         time.Duration
	dialTimeout       time.Duration
	ioTimeout         time.Duration
	minBackoff        time.Duration
	maxBackoff        time.Duration
}

type Stats struct {
	connections       uint64
	dialErrors        uint64
	dropouts          uint64
	reconnects        uint64
	heartbeatsSent    uint64
	heartbeatsAck     uint64
	activeConnections int64
}

func main() {
	addr := flag.String("addr", "106.54.162.98:9000", "server address")
	clients := flag.Int("clients", 1000, "target number of concurrent connections")
	startRate := flag.Int("start-rate", 100, "max new connections started per second")
	heartbeat := flag.Duration("heartbeat", 30*time.Second, "heartbeat interval (PING every N seconds)")
	dialTimeout := flag.Duration("dial-timeout", 5*time.Second, "TCP dial timeout")
	ioTimeout := flag.Duration("io-timeout", 5*time.Second, "IO deadline for heartbeat traffic")
	minBackoff := flag.Duration("min-backoff", 500*time.Millisecond, "initial reconnect backoff")
	maxBackoff := flag.Duration("max-backoff", time.Minute, "maximum reconnect backoff")
	flag.Parse()

	cfg := Config{
		address:           *addr,
		targetConnections: *clients,
		startRate:         *startRate,
		dialLimiter:       newRateLimiter(*startRate),
		heartbeat:         *heartbeat,
		dialTimeout:       *dialTimeout,
		ioTimeout:         *ioTimeout,
		minBackoff:        *minBackoff,
		maxBackoff:        *maxBackoff,
	}

	if cfg.minBackoff <= 0 {
		cfg.minBackoff = 250 * time.Millisecond
	}
	if cfg.maxBackoff < cfg.minBackoff {
		cfg.maxBackoff = cfg.minBackoff
	}

	log.Printf("starting client: target %d connections, heartbeat %s, server %s",
		cfg.targetConnections, cfg.heartbeat, cfg.address)

	stats := &Stats{}
	go reportStats(cfg, stats)

	if cfg.targetConnections <= 0 {
		log.Printf("target connections must be positive")
		return
	}

	initialBackoff := cfg.minBackoff
	if initialBackoff <= 0 {
		initialBackoff = 250 * time.Millisecond
	}

	readySlots := make(chan *Slot, cfg.targetConnections)
	for i := 0; i < cfg.targetConnections; i++ {
		readySlots <- &Slot{id: i + 1, backoff: initialBackoff}
	}

	go connectionManager(cfg, stats, readySlots)

	select {}
}

func connectionManager(cfg Config, stats *Stats, readySlots chan *Slot) {
	for slot := range readySlots {
		if cfg.dialLimiter != nil {
			cfg.dialLimiter.Wait()
		}
		go runSlot(slot, cfg, stats, readySlots)
	}
}

func runSlot(slot *Slot, cfg Config, stats *Stats, readySlots chan *Slot) {
	ping := []byte(pingPayload)
	response := make([]byte, len(ping))

	conn, err := dialWithStats(cfg, stats)
	if err != nil {
		slot.backoff = increaseBackoff(slot.backoff, cfg.maxBackoff)
		scheduleSlot(slot, slot.backoff, readySlots)
		return
	}

	atomic.AddInt64(&stats.activeConnections, 1)
	start := time.Now()
	err = maintain(conn, cfg, ping, response, stats)
	atomic.AddInt64(&stats.activeConnections, -1)
	conn.Close()

	atomic.AddUint64(&stats.dropouts, 1)
	atomic.AddUint64(&stats.reconnects, 1)

	duration := time.Since(start)
	if duration >= cfg.heartbeat {
		slot.backoff = cfg.minBackoff
	} else {
		slot.backoff = increaseBackoff(slot.backoff, cfg.maxBackoff)
	}

	scheduleSlot(slot, slot.backoff, readySlots)
}

func scheduleSlot(slot *Slot, delay time.Duration, readySlots chan *Slot) {
	if delay <= 0 {
		readySlots <- slot
		return
	}

	go func() {
		time.Sleep(delay)
		readySlots <- slot
	}()
}

func dialWithStats(cfg Config, stats *Stats) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", cfg.address, cfg.dialTimeout)
	if err != nil {
		atomic.AddUint64(&stats.dialErrors, 1)
		return nil, err
	}
	atomic.AddUint64(&stats.connections, 1)
	return conn, nil
}

func maintain(conn net.Conn, cfg Config, ping, response []byte, stats *Stats) error {
	ticker := time.NewTicker(cfg.heartbeat)
	defer ticker.Stop()

	if err := sendHeartbeat(conn, cfg, ping, response, stats); err != nil {
		return err
	}

	for range ticker.C {
		if err := sendHeartbeat(conn, cfg, ping, response, stats); err != nil {
			return err
		}
	}
	return nil
}

func sendHeartbeat(conn net.Conn, cfg Config, ping, response []byte, stats *Stats) error {
	deadline := time.Now().Add(cfg.ioTimeout)
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	if _, err := conn.Write(ping); err != nil {
		return err
	}

	atomic.AddUint64(&stats.heartbeatsSent, 1)

	if err := conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	if _, err := io.ReadFull(conn, response[:len(ping)]); err != nil {
		return err
	}

	atomic.AddUint64(&stats.heartbeatsAck, 1)
	return nil
}

func increaseBackoff(current, maximum time.Duration) time.Duration {
	if current <= 0 {
		current = 100 * time.Millisecond
	}
	current *= 2
	if current > maximum {
		return maximum
	}
	return current
}

func reportStats(cfg Config, stats *Stats) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		log.Printf("stats target=%d active=%d connected=%d drops=%d heartbeats=%d ack=%d dials=%d reconnects=%d",
			cfg.targetConnections,
			atomic.LoadInt64(&stats.activeConnections),
			atomic.LoadUint64(&stats.connections),
			atomic.LoadUint64(&stats.dropouts),
			atomic.LoadUint64(&stats.heartbeatsSent),
			atomic.LoadUint64(&stats.heartbeatsAck),
			atomic.LoadUint64(&stats.dialErrors),
			atomic.LoadUint64(&stats.reconnects),
		)
	}
}
