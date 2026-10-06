package capacity

import (
	"context"
	"errors"
	"log"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type RateLimiter struct {
	tokens chan struct{}
}

// newRateLimiter refills in limiterTick batches via integer carry — a per-token
// ticker needs sub-100µs ticks at high rates and weak CPUs miss them.
func newRateLimiter(ctx context.Context, rate int) *RateLimiter {
	rl := &RateLimiter{tokens: make(chan struct{}, bucketCapacity(rate))}
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
				var n int
				n, carry = refill(carry, rate, ticksPerSecond)
				for ; n > 0; n-- {
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

// bucketCapacity holds 100ms worth of tokens: a full one-second bucket plus a
// second of refill would let a burst after an idle spell reach twice the rate.
func bucketCapacity(rate int) int {
	return max(1, rate/10)
}

// refill returns the tokens due this tick and the remainder carried to the next,
// so ticksPerSecond ticks issue exactly rate tokens with no drift.
func refill(carry, rate, ticksPerSecond int) (tokens, nextCarry int) {
	carry += rate
	return carry / ticksPerSecond, carry % ticksPerSecond
}

func (rl *RateLimiter) Wait(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-rl.tokens:
		return true
	}
}

// Stats are the run's counters, all updated atomically from many goroutines.
type Stats struct {
	dialAttempts      uint64
	connections       uint64
	dialErrors        uint64
	dialErrorsHost    uint64
	handshakeErrors   uint64
	rejectedBusy      uint64
	rejectedQuota     uint64
	rejectedAuth      uint64
	rejectedOther     uint64
	dropTimeouts      uint64
	dropClosed        uint64
	dropErrors        uint64
	badAcks           uint64
	dropNodeClosed    uint64
	nodeClosedIdle    uint64
	nodeClosedSession uint64
	nodeClosedOther   uint64
	heartbeatsSent    uint64
	heartbeatsAck     uint64
	intervalAcks      uint64
	sessionNanos      uint64
	activeConnections int64
	lastError         atomic.Pointer[string]
	family            atomic.Pointer[string]
	rtt               *rttHistogram
	host              hostLimits // set before any goroutine starts, read-only after
}

// snapshot is one read of the counters. Drops are not counted separately but
// derived from the five buckets, so the total always equals their sum.
type snapshot struct {
	dialAttempts, connects, dialErrors, dialErrorsHost, handshakeErrors uint64
	rejectedBusy, rejectedQuota, rejectedAuth, rejectedOther            uint64
	dropTimeouts, dropClosed, dropErrors, dropBadAcks, dropNodeClosed   uint64
	nodeClosedIdle, nodeClosedSession, nodeClosedOther                  uint64
	heartbeatsSent, heartbeatsAck, intervalAcks                         uint64
	sessionNanos                                                        uint64
	active                                                              int64
	lastError, family                                                   string
	host                                                                hostLimits
}

func (s snapshot) drops() uint64 {
	return s.dropTimeouts + s.dropClosed + s.dropErrors + s.dropBadAcks + s.dropNodeClosed
}

func (s snapshot) rejected() uint64 {
	return s.rejectedBusy + s.rejectedQuota + s.rejectedAuth + s.rejectedOther
}

func (st *Stats) snapshot() snapshot {
	load := atomic.LoadUint64
	s := snapshot{
		dialAttempts:      load(&st.dialAttempts),
		connects:          load(&st.connections),
		dialErrors:        load(&st.dialErrors),
		dialErrorsHost:    load(&st.dialErrorsHost),
		handshakeErrors:   load(&st.handshakeErrors),
		rejectedBusy:      load(&st.rejectedBusy),
		rejectedQuota:     load(&st.rejectedQuota),
		rejectedAuth:      load(&st.rejectedAuth),
		rejectedOther:     load(&st.rejectedOther),
		dropTimeouts:      load(&st.dropTimeouts),
		dropClosed:        load(&st.dropClosed),
		dropErrors:        load(&st.dropErrors),
		dropBadAcks:       load(&st.badAcks),
		dropNodeClosed:    load(&st.dropNodeClosed),
		nodeClosedIdle:    load(&st.nodeClosedIdle),
		nodeClosedSession: load(&st.nodeClosedSession),
		nodeClosedOther:   load(&st.nodeClosedOther),
		heartbeatsSent:    load(&st.heartbeatsSent),
		heartbeatsAck:     load(&st.heartbeatsAck),
		intervalAcks:      load(&st.intervalAcks),
		sessionNanos:      load(&st.sessionNanos),
		active:            atomic.LoadInt64(&st.activeConnections),
		host:              st.host,
	}
	if p := st.lastError.Load(); p != nil {
		s.lastError = *p
	}
	if p := st.family.Load(); p != nil {
		s.family = *p
	}
	return s
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

// rttHistogram is a lock-free latency histogram: log-scale buckets ~10% apart
// (10µs–600s), so percentiles carry ~10% resolution without storing samples.
// All mutation is atomic — concurrent Record is safe; a nil histogram no-ops.
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

type finalStats struct {
	snap      snapshot
	rttCounts []uint64
	rttTotal  uint64
}

// reportStats logs a progress line per second until ctx ends, then logs the exit
// reason and sends the final counters with the whole-run RTT distribution on done;
// the pool waits for it, so the result cannot be built before the last read.
func reportStats(ctx context.Context, target int, stats *Stats, done chan<- finalStats) {
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
			done <- finalStats{snap: stats.snapshot(), rttCounts: cumCounts, rttTotal: cumTotal}
			return
		case <-ticker.C:
			counts, n := stats.rtt.collect()
			cumCounts, cumTotal = addCounts(cumCounts, cumTotal, counts, n)
			s := stats.snapshot()
			log.Printf("stats target=%d active=%d dial_attempts=%d connects=%d dial_errors=%d rejected=%d drops=%d drop_timeout=%d drop_closed=%d drop_error=%d drop_node=%d heartbeats=%d ack=%d bad_ack=%d rtt_samples=%d rtt_p50=%s rtt_p95=%s rtt_p99=%s",
				target, s.active, s.dialAttempts, s.connects, s.dialErrors, s.rejected(),
				s.drops(), s.dropTimeouts, s.dropClosed, s.dropErrors, s.dropNodeClosed,
				s.heartbeatsSent, s.heartbeatsAck, s.dropBadAcks, n,
				stats.rtt.quantile(counts, n, 0.50).Round(time.Microsecond),
				stats.rtt.quantile(counts, n, 0.95).Round(time.Microsecond),
				stats.rtt.quantile(counts, n, 0.99).Round(time.Microsecond),
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
