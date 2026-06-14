package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"sort"
	"sync/atomic"
	"syscall"
	"time"
)

const pingPayload = "PING"
const maxStartRate = 100000
const maxClients = 1000000

// errBadAck marks a heartbeat reply that did not match the ping payload. It is
// already counted in Stats.badAcks, so recordDropReason skips re-bucketing it.
var errBadAck = errors.New("bad heartbeat ack")

type RateLimiter struct {
	tokens chan struct{}
}

func newRateLimiter(rate int) *RateLimiter {
	interval := time.Second / time.Duration(rate)

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

func (rl *RateLimiter) Wait(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-rl.tokens:
		return true
	}
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
	tcpKeepAlive      time.Duration
	logDrops          bool
}

type Stats struct {
	dialAttempts      uint64
	connections       uint64
	dialErrors        uint64
	dropouts          uint64
	dropTimeouts      uint64
	dropClosed        uint64
	dropErrors        uint64
	retries           uint64
	heartbeatsSent    uint64
	heartbeatsAck     uint64
	badAcks           uint64
	activeConnections int64
	rtt               *rttHistogram
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9000", "server address")
	clients := flag.Int("clients", 1000, "target number of concurrent connections")
	startRate := flag.Int("start-rate", 100, "max new connections started per second")
	heartbeat := flag.Duration("heartbeat", 30*time.Second, "heartbeat interval (PING every N seconds)")
	dialTimeout := flag.Duration("dial-timeout", 5*time.Second, "TCP dial timeout")
	ioTimeout := flag.Duration("io-timeout", 5*time.Second, "IO deadline for heartbeat traffic")
	minBackoff := flag.Duration("min-backoff", 500*time.Millisecond, "initial reconnect backoff")
	maxBackoff := flag.Duration("max-backoff", time.Minute, "maximum reconnect backoff")
	tcpKeepAlive := flag.Duration("tcp-keepalive", 30*time.Second, "TCP keepalive probe interval; set <=0 to disable")
	logDrops := flag.Bool("log-drops", false, "log every connection drop with its reason")
	flag.Parse()

	cfg := Config{
		address:           *addr,
		targetConnections: *clients,
		startRate:         *startRate,
		heartbeat:         *heartbeat,
		dialTimeout:       *dialTimeout,
		ioTimeout:         *ioTimeout,
		minBackoff:        *minBackoff,
		maxBackoff:        *maxBackoff,
		tcpKeepAlive:      *tcpKeepAlive,
		logDrops:          *logDrops,
	}

	if err := validateConfig(cfg); err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}
	cfg.dialLimiter = newRateLimiter(cfg.startRate)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("starting client: target=%d heartbeat=%s server=%s start_rate=%d tcp_keepalive=%s",
		cfg.targetConnections, cfg.heartbeat, cfg.address, cfg.startRate, cfg.tcpKeepAlive)

	stats := &Stats{rtt: newRttHistogram()}
	go reportStats(ctx, cfg, stats)

	initialBackoff := cfg.minBackoff
	readySlots := make(chan *Slot, cfg.targetConnections)
	for i := 0; i < cfg.targetConnections; i++ {
		readySlots <- &Slot{id: i + 1, backoff: initialBackoff}
	}

	go connectionManager(ctx, cfg, stats, readySlots)

	<-ctx.Done()
	log.Printf("shutdown requested")
}

func validateConfig(cfg Config) error {
	if cfg.address == "" {
		return fmt.Errorf("addr must not be empty")
	}
	if cfg.targetConnections <= 0 {
		return fmt.Errorf("clients must be positive")
	}
	if cfg.targetConnections > maxClients {
		return fmt.Errorf("clients must be <= %d", maxClients)
	}
	if cfg.startRate <= 0 {
		return fmt.Errorf("start-rate must be positive")
	}
	if cfg.startRate > maxStartRate {
		return fmt.Errorf("start-rate must be <= %d", maxStartRate)
	}
	if cfg.heartbeat <= 0 {
		return fmt.Errorf("heartbeat must be positive")
	}
	if cfg.dialTimeout <= 0 {
		return fmt.Errorf("dial-timeout must be positive")
	}
	if cfg.ioTimeout <= 0 {
		return fmt.Errorf("io-timeout must be positive")
	}
	if cfg.minBackoff <= 0 {
		return fmt.Errorf("min-backoff must be positive")
	}
	if cfg.maxBackoff <= 0 {
		return fmt.Errorf("max-backoff must be positive")
	}
	if cfg.maxBackoff < cfg.minBackoff {
		return fmt.Errorf("max-backoff must be >= min-backoff")
	}
	return nil
}

func connectionManager(ctx context.Context, cfg Config, stats *Stats, readySlots chan *Slot) {
	for {
		select {
		case <-ctx.Done():
			return
		case slot := <-readySlots:
			if !cfg.dialLimiter.Wait(ctx) {
				return
			}
			go runSlot(ctx, slot, cfg, stats, readySlots)
		}
	}
}

func runSlot(ctx context.Context, slot *Slot, cfg Config, stats *Stats, readySlots chan *Slot) {
	ping := []byte(pingPayload)
	response := make([]byte, len(ping))

	conn, err := dialWithStats(ctx, cfg, stats)
	if err != nil {
		slot.backoff = increaseBackoff(slot.backoff, cfg.maxBackoff)
		atomic.AddUint64(&stats.retries, 1)
		scheduleSlot(ctx, slot, jitter(slot.backoff), readySlots)
		return
	}

	atomic.AddInt64(&stats.activeConnections, 1)
	start := time.Now()
	err = maintain(ctx, conn, cfg, ping, response, stats)
	atomic.AddInt64(&stats.activeConnections, -1)
	conn.Close()

	if ctx.Err() != nil {
		return
	}

	atomic.AddUint64(&stats.dropouts, 1)
	atomic.AddUint64(&stats.retries, 1)
	recordDropReason(err, stats)

	duration := time.Since(start)
	if cfg.logDrops {
		log.Printf("slot %d dropped after %s: %v", slot.id, duration.Round(time.Millisecond), err)
	}
	if duration >= cfg.heartbeat {
		slot.backoff = cfg.minBackoff
	} else {
		slot.backoff = increaseBackoff(slot.backoff, cfg.maxBackoff)
	}

	scheduleSlot(ctx, slot, jitter(slot.backoff), readySlots)
}

func scheduleSlot(ctx context.Context, slot *Slot, delay time.Duration, readySlots chan *Slot) {
	select {
	case <-ctx.Done():
		return
	default:
	}

	if delay <= 0 {
		select {
		case readySlots <- slot:
		case <-ctx.Done():
		}
		return
	}

	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
			select {
			case <-ctx.Done():
				return
			default:
			}
			select {
			case readySlots <- slot:
			case <-ctx.Done():
			}
		case <-ctx.Done():
		}
	}()
}

func dialWithStats(ctx context.Context, cfg Config, stats *Stats) (net.Conn, error) {
	atomic.AddUint64(&stats.dialAttempts, 1)
	dialer := net.Dialer{
		Timeout:   cfg.dialTimeout,
		KeepAlive: cfg.tcpKeepAlive,
	}
	if cfg.tcpKeepAlive <= 0 {
		dialer.KeepAlive = -1
	}

	conn, err := dialer.DialContext(ctx, "tcp", cfg.address)
	if err != nil {
		atomic.AddUint64(&stats.dialErrors, 1)
		return nil, err
	}
	atomic.AddUint64(&stats.connections, 1)
	return conn, nil
}

func maintain(ctx context.Context, conn net.Conn, cfg Config, ping, response []byte, stats *Stats) error {
	ticker := time.NewTicker(cfg.heartbeat)
	defer ticker.Stop()

	if err := sendHeartbeat(conn, cfg, ping, response, stats); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := sendHeartbeat(conn, cfg, ping, response, stats); err != nil {
				return err
			}
		}
	}
}

func sendHeartbeat(conn net.Conn, cfg Config, ping, response []byte, stats *Stats) error {
	start := time.Now()
	if err := conn.SetWriteDeadline(start.Add(cfg.ioTimeout)); err != nil {
		return err
	}
	if err := writeFull(conn, ping); err != nil {
		return err
	}

	atomic.AddUint64(&stats.heartbeatsSent, 1)

	if err := conn.SetReadDeadline(time.Now().Add(cfg.ioTimeout)); err != nil {
		return err
	}
	if _, err := io.ReadFull(conn, response[:len(ping)]); err != nil {
		return err
	}
	if !bytes.Equal(response[:len(ping)], ping) {
		atomic.AddUint64(&stats.badAcks, 1)
		return fmt.Errorf("%w: got %q want %q", errBadAck, response[:len(ping)], ping)
	}

	atomic.AddUint64(&stats.heartbeatsAck, 1)
	stats.rtt.Record(time.Since(start))
	return nil
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

// recordDropReason buckets a dropped connection's error so the periodic stats
// line attributes it instead of discarding it. The buckets partition dropouts:
// timeouts (likely silent NAT/middlebox drops), closed (peer sent EOF/FIN),
// and errors (resets and everything else); bad acks are already counted.
func recordDropReason(err error, stats *Stats) {
	switch {
	case err == nil:
		return
	case errors.Is(err, errBadAck):
		// Already counted in Stats.badAcks by sendHeartbeat.
	case isTimeout(err):
		atomic.AddUint64(&stats.dropTimeouts, 1)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		atomic.AddUint64(&stats.dropClosed, 1)
	default:
		atomic.AddUint64(&stats.dropErrors, 1)
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// jitter applies equal jitter to a reconnect delay, returning a random duration
// in [d/2, d]. Spreading the waits desynchronizes the reconnect waves that follow
// a mass drop (e.g. a server restart), smoothing load spikes. The caller keeps
// slot.backoff itself un-jittered so the exponential progression stays clean.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
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

// rttHistogram is a lock-free, fixed-bucket latency histogram. Buckets are
// spaced ~10% apart on a log scale from 10µs to 600s, giving roughly 10%
// resolution on reported percentiles without storing individual samples. All
// mutation is via sync/atomic (matching the rest of Stats), so every connection
// goroutine can Record concurrently. A nil *rttHistogram is a no-op, so tests
// can use a zero-value Stats without wiring one up.
type rttHistogram struct {
	bounds  []time.Duration // ascending upper bounds
	buckets []uint64        // len(bounds)+1; buckets[len(bounds)] holds the overflow
}

func newRttHistogram() *rttHistogram {
	const (
		minBound = 10 * time.Microsecond
		maxBound = 600 * time.Second
	)
	var bounds []time.Duration
	for v := minBound; v < maxBound; v += v / 10 {
		bounds = append(bounds, v)
	}
	return &rttHistogram{bounds: bounds, buckets: make([]uint64, len(bounds)+1)}
}

// Record adds one RTT sample. Safe for concurrent use; nil-safe.
func (h *rttHistogram) Record(d time.Duration) {
	if h == nil {
		return
	}
	if d < 0 {
		d = 0
	}
	i := sort.Search(len(h.bounds), func(i int) bool { return d <= h.bounds[i] })
	atomic.AddUint64(&h.buckets[i], 1)
}

// collect atomically reads and zeroes every bucket, returning the per-interval
// distribution and total sample count. Concurrent Records are never lost: each
// lands either before its swap (this interval) or after it (the next one).
func (h *rttHistogram) collect() (counts []uint64, total uint64) {
	if h == nil {
		return nil, 0
	}
	counts = make([]uint64, len(h.buckets))
	for i := range h.buckets {
		c := atomic.SwapUint64(&h.buckets[i], 0)
		counts[i] = c
		total += c
	}
	return counts, total
}

// quantile returns the nearest-rank q-quantile (q in [0,1]) of a distribution
// from collect, reported as the upper bound of the containing bucket. Returns 0
// when there are no samples.
func (h *rttHistogram) quantile(counts []uint64, total uint64, q float64) time.Duration {
	if h == nil || total == 0 {
		return 0
	}
	target := uint64(math.Ceil(q * float64(total)))
	if target == 0 {
		target = 1
	}
	var cum uint64
	for i, c := range counts {
		cum += c
		if cum >= target {
			if i < len(h.bounds) {
				return h.bounds[i]
			}
			return h.bounds[len(h.bounds)-1]
		}
	}
	return h.bounds[len(h.bounds)-1]
}

func reportStats(ctx context.Context, cfg Config, stats *Stats) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rttCounts, rttSamples := stats.rtt.collect()
			p50 := stats.rtt.quantile(rttCounts, rttSamples, 0.50)
			p95 := stats.rtt.quantile(rttCounts, rttSamples, 0.95)
			p99 := stats.rtt.quantile(rttCounts, rttSamples, 0.99)
			log.Printf("stats target=%d active=%d dial_attempts=%d connected=%d dial_errors=%d drops=%d drop_timeout=%d drop_closed=%d drop_error=%d heartbeats=%d ack=%d bad_ack=%d retries=%d rtt_samples=%d rtt_p50=%s rtt_p95=%s rtt_p99=%s",
				cfg.targetConnections,
				atomic.LoadInt64(&stats.activeConnections),
				atomic.LoadUint64(&stats.dialAttempts),
				atomic.LoadUint64(&stats.connections),
				atomic.LoadUint64(&stats.dialErrors),
				atomic.LoadUint64(&stats.dropouts),
				atomic.LoadUint64(&stats.dropTimeouts),
				atomic.LoadUint64(&stats.dropClosed),
				atomic.LoadUint64(&stats.dropErrors),
				atomic.LoadUint64(&stats.heartbeatsSent),
				atomic.LoadUint64(&stats.heartbeatsAck),
				atomic.LoadUint64(&stats.badAcks),
				atomic.LoadUint64(&stats.retries),
				rttSamples,
				p50.Round(time.Microsecond),
				p95.Round(time.Microsecond),
				p99.Round(time.Microsecond),
			)
		}
	}
}
