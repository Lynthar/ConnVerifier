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
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const pingPayload = "PING"
const maxStartRate = 100000
const maxClients = 1000000
const limiterTick = 10 * time.Millisecond
const dialLogWindow = 10 * time.Second

// errBadAck marks a heartbeat reply that did not match the ping payload. It is
// already counted in Stats.badAcks, so recordDropReason skips re-bucketing it.
var errBadAck = errors.New("bad heartbeat ack")

type RateLimiter struct {
	tokens chan struct{}
}

// newRateLimiter refills in limiterTick batches via integer carry — a per-token
// ticker needs sub-100µs ticks at high rates and weak CPUs miss them. Capacity
// is 100ms worth of tokens so an idle spell cannot bank a burst past the rate.
func newRateLimiter(ctx context.Context, rate int) *RateLimiter {
	capacity := rate / 10
	if capacity < 1 {
		capacity = 1
	}
	rl := &RateLimiter{tokens: make(chan struct{}, capacity)}
	go func() {
		ticksPerSecond := int(time.Second / limiterTick)
		ticker := time.NewTicker(limiterTick)
		defer ticker.Stop()
		carry := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				carry += rate
				for ; carry >= ticksPerSecond; carry -= ticksPerSecond {
					select {
					case rl.tokens <- struct{}{}:
					default: // bucket full: the excess token is discarded
					}
				}
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
	dialLog           *dialLogger
	heartbeat         time.Duration
	dialTimeout       time.Duration
	ioTimeout         time.Duration
	minBackoff        time.Duration
	maxBackoff        time.Duration
	tcpKeepAlive      time.Duration
	duration          time.Duration
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
	heartbeatsSent    uint64
	heartbeatsAck     uint64
	badAcks           uint64
	sessionNanos      uint64
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
	tcpKeepAlive := flag.Duration("tcp-keepalive", 0, "TCP keepalive probe interval; <=0 disables (keepalive refreshes NAT mappings and masks the idle timeout under test)")
	duration := flag.Duration("duration", 0, "stop after this long and print the summary; <=0 runs until interrupted")
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
		duration:          *duration,
		logDrops:          *logDrops,
	}

	if err := validateConfig(cfg); err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.duration)
		defer cancel()
	}
	cfg.dialLimiter = newRateLimiter(ctx, cfg.startRate)
	cfg.dialLog = newDialLogger()

	log.Printf("starting client: target=%d heartbeat=%s server=%s start_rate=%d tcp_keepalive=%s",
		cfg.targetConnections, cfg.heartbeat, cfg.address, cfg.startRate, cfg.tcpKeepAlive)

	stats := &Stats{rtt: newRttHistogram()}
	reporterDone := make(chan struct{})
	go reportStats(ctx, cfg, stats, reporterDone)

	initialBackoff := cfg.minBackoff
	readySlots := make(chan *Slot, cfg.targetConnections)
	for i := 0; i < cfg.targetConnections; i++ {
		readySlots <- &Slot{id: i + 1, backoff: initialBackoff}
	}

	go connectionManager(ctx, cfg, stats, readySlots)

	<-reporterDone
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
		scheduleSlot(ctx, slot, jitter(slot.backoff), readySlots)
		return
	}

	atomic.AddInt64(&stats.activeConnections, 1)
	start := time.Now()
	survived, err := maintain(ctx, conn, cfg, ping, response, stats)
	atomic.AddInt64(&stats.activeConnections, -1)
	conn.Close()

	if ctx.Err() != nil {
		return
	}

	duration := time.Since(start)
	atomic.AddUint64(&stats.dropouts, 1)
	atomic.AddUint64(&stats.sessionNanos, uint64(duration))
	recordDropReason(err, stats)

	if cfg.logDrops {
		log.Printf("slot %d dropped after %s: %v", slot.id, duration.Round(time.Millisecond), err)
	}
	if survived {
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

// dialLogger rate-limits dial-failure log lines to one per distinct error text
// per dialLogWindow, keeping the cause visible by default without flooding when
// thousands of dials fail the same way. A nil *dialLogger logs nothing.
type dialLogger struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newDialLogger() *dialLogger {
	return &dialLogger{last: make(map[string]time.Time)}
}

// shouldLog reports whether msg is due for logging at now, recording it if so.
func (l *dialLogger) shouldLog(msg string, now time.Time) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.last[msg]; ok && now.Sub(t) < dialLogWindow {
		return false
	}
	l.last[msg] = now
	return true
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
		if ctx.Err() != nil {
			return nil, err // shutdown cancellation, not a dial failure
		}
		atomic.AddUint64(&stats.dialErrors, 1)
		if cfg.dialLog.shouldLog(err.Error(), time.Now()) {
			log.Printf("dial failed: %v", err)
		}
		return nil, err
	}
	atomic.AddUint64(&stats.connections, 1)
	return conn, nil
}

// maintain runs the heartbeat loop until the connection breaks or ctx ends,
// reporting whether any interval heartbeat was acked — proof the connection
// survived a full heartbeat interval, which is what resets the backoff.
func maintain(ctx context.Context, conn net.Conn, cfg Config, ping, response []byte, stats *Stats) (survived bool, err error) {
	ticker := time.NewTicker(cfg.heartbeat)
	defer ticker.Stop()

	if err := sendHeartbeat(conn, cfg, ping, response, stats); err != nil {
		return false, err
	}

	for {
		select {
		case <-ctx.Done():
			return survived, ctx.Err()
		case <-ticker.C:
			if err := sendHeartbeat(conn, cfg, ping, response, stats); err != nil {
				return survived, err
			}
			survived = true
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

// reportStats logs the per-second stats line until ctx ends, then prints the
// exit reason and the cumulative run summary (with whole-run RTT distribution)
// and closes done; main blocks on done so the process cannot exit before it.
func reportStats(ctx context.Context, cfg Config, stats *Stats, done chan<- struct{}) {
	defer close(done)
	start := time.Now()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var cumCounts []uint64
	var cumTotal uint64
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				log.Printf("test duration elapsed")
			} else {
				log.Printf("shutdown requested")
			}
			counts, n := stats.rtt.collect()
			cumCounts, cumTotal = addCounts(cumCounts, cumTotal, counts, n)
			printSummary(stats, time.Since(start), cumCounts, cumTotal)
			return
		case <-ticker.C:
			counts, n := stats.rtt.collect()
			cumCounts, cumTotal = addCounts(cumCounts, cumTotal, counts, n)
			p50 := stats.rtt.quantile(counts, n, 0.50)
			p95 := stats.rtt.quantile(counts, n, 0.95)
			p99 := stats.rtt.quantile(counts, n, 0.99)
			log.Printf("stats target=%d active=%d dial_attempts=%d connects=%d dial_errors=%d drops=%d drop_timeout=%d drop_closed=%d drop_error=%d heartbeats=%d ack=%d bad_ack=%d rtt_samples=%d rtt_p50=%s rtt_p95=%s rtt_p99=%s",
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
				n,
				p50.Round(time.Microsecond),
				p95.Round(time.Microsecond),
				p99.Round(time.Microsecond),
			)
		}
	}
}

func addCounts(cum []uint64, cumTotal uint64, counts []uint64, n uint64) ([]uint64, uint64) {
	if cum == nil {
		cum = make([]uint64, len(counts))
	}
	for i, c := range counts {
		cum[i] += c
	}
	return cum, cumTotal + n
}

func printSummary(stats *Stats, runtime time.Duration, rttCounts []uint64, rttTotal uint64) {
	drops := atomic.LoadUint64(&stats.dropouts)
	avgDropped := "n/a"
	if drops > 0 {
		avg := time.Duration(atomic.LoadUint64(&stats.sessionNanos) / drops)
		avgDropped = avg.Round(time.Millisecond).String()
	}
	log.Printf("summary runtime=%s connects=%d dial_errors=%d drops=%d drop_timeout=%d drop_closed=%d drop_error=%d heartbeats=%d ack=%d bad_ack=%d avg_dropped_session=%s rtt_samples=%d rtt_p50=%s rtt_p95=%s rtt_p99=%s",
		runtime.Round(time.Second),
		atomic.LoadUint64(&stats.connections),
		atomic.LoadUint64(&stats.dialErrors),
		drops,
		atomic.LoadUint64(&stats.dropTimeouts),
		atomic.LoadUint64(&stats.dropClosed),
		atomic.LoadUint64(&stats.dropErrors),
		atomic.LoadUint64(&stats.heartbeatsSent),
		atomic.LoadUint64(&stats.heartbeatsAck),
		atomic.LoadUint64(&stats.badAcks),
		avgDropped,
		rttTotal,
		stats.rtt.quantile(rttCounts, rttTotal, 0.50).Round(time.Microsecond),
		stats.rtt.quantile(rttCounts, rttTotal, 0.95).Round(time.Microsecond),
		stats.rtt.quantile(rttCounts, rttTotal, 0.99).Round(time.Microsecond),
	)
}
