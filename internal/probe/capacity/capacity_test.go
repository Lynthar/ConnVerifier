package capacity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func validClientConfig() Config {
	return Config{
		address:           "127.0.0.1:9000",
		targetConnections: 1,
		startRate:         1,
		heartbeat:         time.Second,
		dialTimeout:       time.Second,
		ioTimeout:         time.Second,
		minBackoff:        100 * time.Millisecond,
		maxBackoff:        time.Second,
	}
}

func TestValidateConfig(t *testing.T) {
	cfg := validClientConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	tests := []struct {
		name   string
		update func(*Config)
	}{
		{name: "empty address", update: func(c *Config) { c.address = "" }},
		{name: "zero clients", update: func(c *Config) { c.targetConnections = 0 }},
		{name: "excessive clients", update: func(c *Config) { c.targetConnections = maxClients + 1 }},
		{name: "zero start rate", update: func(c *Config) { c.startRate = 0 }},
		{name: "excessive start rate", update: func(c *Config) { c.startRate = maxStartRate + 1 }},
		{name: "zero heartbeat", update: func(c *Config) { c.heartbeat = 0 }},
		{name: "zero dial timeout", update: func(c *Config) { c.dialTimeout = 0 }},
		{name: "zero io timeout", update: func(c *Config) { c.ioTimeout = 0 }},
		{name: "zero min backoff", update: func(c *Config) { c.minBackoff = 0 }},
		{name: "max backoff below min", update: func(c *Config) { c.maxBackoff = c.minBackoff - time.Millisecond }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validClientConfig()
			tt.update(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("invalid config accepted")
			}
		})
	}
}

func TestIncreaseBackoff(t *testing.T) {
	if got := increaseBackoff(100*time.Millisecond, time.Second); got != 200*time.Millisecond {
		t.Fatalf("unexpected backoff: %s", got)
	}
	if got := increaseBackoff(800*time.Millisecond, time.Second); got != time.Second {
		t.Fatalf("backoff should cap at maximum, got %s", got)
	}
}

func TestJitter(t *testing.T) {
	if got := jitter(0); got != 0 {
		t.Fatalf("jitter(0) = %s, want 0", got)
	}
	if got := jitter(-time.Second); got != 0 {
		t.Fatalf("jitter(negative) = %s, want 0", got)
	}

	d := 800 * time.Millisecond
	for i := 0; i < 1000; i++ {
		got := jitter(d)
		if got < d/2 || got > d {
			t.Fatalf("jitter(%s) = %s, want within [%s, %s]", d, got, d/2, d)
		}
	}
}

func TestSendHeartbeatCountsAck(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, len(pingPayload))
		if _, err := io.ReadFull(server, buf); err != nil {
			done <- err
			return
		}
		_, err := server.Write(buf)
		done <- err
	}()

	stats := &Stats{}
	ping := []byte(pingPayload)
	response := make([]byte, len(ping))
	if err := sendHeartbeat(client, validClientConfig(), ping, response, stats); err != nil {
		t.Fatalf("sendHeartbeat failed: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("server side failed: %v", err)
	}
	if got := atomic.LoadUint64(&stats.heartbeatsSent); got != 1 {
		t.Fatalf("heartbeatsSent = %d, want 1", got)
	}
	if got := atomic.LoadUint64(&stats.heartbeatsAck); got != 1 {
		t.Fatalf("heartbeatsAck = %d, want 1", got)
	}
}

func TestSendHeartbeatRejectsBadAck(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, len(pingPayload))
		if _, err := io.ReadFull(server, buf); err != nil {
			done <- err
			return
		}
		_, err := server.Write([]byte("PONG"))
		done <- err
	}()

	stats := &Stats{}
	ping := []byte(pingPayload)
	response := make([]byte, len(ping))
	if err := sendHeartbeat(client, validClientConfig(), ping, response, stats); !errors.Is(err, errBadAck) {
		t.Fatalf("sendHeartbeat error = %v, want errBadAck", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("server side failed: %v", err)
	}
	if got := atomic.LoadUint64(&stats.heartbeatsAck); got != 0 {
		t.Fatalf("heartbeatsAck = %d, want 0", got)
	}
}

type fakeTimeoutError struct{}

func (fakeTimeoutError) Error() string   { return "i/o timeout" }
func (fakeTimeoutError) Timeout() bool   { return true }
func (fakeTimeoutError) Temporary() bool { return true }

func TestRecordDropReason(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want func(*Stats) uint64
	}{
		{"timeout", fakeTimeoutError{}, func(s *Stats) uint64 { return s.dropTimeouts }},
		{"wrapped timeout", fmt.Errorf("read: %w", fakeTimeoutError{}), func(s *Stats) uint64 { return s.dropTimeouts }},
		{"eof", io.EOF, func(s *Stats) uint64 { return s.dropClosed }},
		{"unexpected eof", io.ErrUnexpectedEOF, func(s *Stats) uint64 { return s.dropClosed }},
		{"wrapped eof", fmt.Errorf("read: %w", io.EOF), func(s *Stats) uint64 { return s.dropClosed }},
		{"other", errors.New("connection reset by peer"), func(s *Stats) uint64 { return s.dropErrors }},
		{"bad ack", fmt.Errorf("%w: got %q want %q", errBadAck, "PONG", "PING"), func(s *Stats) uint64 { return s.badAcks }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stats := &Stats{}
			recordDropReason(tt.err, stats)
			if got := tt.want(stats); got != 1 {
				t.Fatalf("expected reason bucket = 1, got %d", got)
			}
			if total := stats.snapshot().drops(); total != 1 {
				t.Fatalf("expected exactly one bucket incremented, got total %d", total)
			}
		})
	}
}

// The drop total is derived from the buckets, so it equals their sum by
// construction; this pins that no drop kind bypasses the buckets.
func TestSnapshotDropsEqualBuckets(t *testing.T) {
	stats := &Stats{}
	for _, err := range []error{
		fakeTimeoutError{}, io.EOF, errors.New("connection reset by peer"),
		fmt.Errorf("%w: got %q want %q", errBadAck, "PONG", "PING"), nil,
	} {
		recordDropReason(err, stats)
	}
	s := stats.snapshot()
	sum := s.dropTimeouts + s.dropClosed + s.dropErrors + s.dropBadAcks
	if s.drops() != sum || sum != 5 {
		t.Fatalf("drops = %d, bucket sum = %d, want both 5", s.drops(), sum)
	}
}

func TestRttHistogramBounds(t *testing.T) {
	h := newRttHistogram()
	if len(h.bounds) == 0 {
		t.Fatal("no bounds generated")
	}
	if len(h.buckets) != len(h.bounds)+1 {
		t.Fatalf("buckets=%d, want bounds+1=%d", len(h.buckets), len(h.bounds)+1)
	}
	for i := 1; i < len(h.bounds); i++ {
		if h.bounds[i] <= h.bounds[i-1] {
			t.Fatalf("bounds not strictly ascending at %d: %s <= %s", i, h.bounds[i], h.bounds[i-1])
		}
	}
}

func TestRttHistogramQuantile(t *testing.T) {
	h := newRttHistogram()
	for i := 0; i < 95; i++ {
		h.Record(10 * time.Millisecond)
	}
	for i := 0; i < 5; i++ {
		h.Record(500 * time.Millisecond)
	}

	counts, total := h.collect()
	if total != 100 {
		t.Fatalf("total=%d, want 100", total)
	}
	// Buckets are ~10% wide, and quantile reports the bucket's upper bound, so
	// the estimate sits in [value, value*1.1).
	if p50 := h.quantile(counts, total, 0.50); p50 < 10*time.Millisecond || p50 > 11*time.Millisecond {
		t.Fatalf("p50=%s, want ~10ms", p50)
	}
	if p99 := h.quantile(counts, total, 0.99); p99 < 500*time.Millisecond || p99 > 550*time.Millisecond {
		t.Fatalf("p99=%s, want ~500ms", p99)
	}

	// collect must have reset the buckets.
	if _, total2 := h.collect(); total2 != 0 {
		t.Fatalf("collect did not reset, total=%d", total2)
	}
}

func TestRttHistogramNilSafe(t *testing.T) {
	var h *rttHistogram
	h.Record(time.Millisecond) // must not panic
	counts, total := h.collect()
	if total != 0 || counts != nil {
		t.Fatalf("nil collect should be empty, got total=%d", total)
	}
	if got := h.quantile(counts, total, 0.5); got != 0 {
		t.Fatalf("nil quantile should be 0, got %s", got)
	}
}

func TestRttHistogramConcurrentRecord(t *testing.T) {
	h := newRttHistogram()
	const goroutines, perGoroutine = 8, 1000

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				h.Record(time.Duration(i%50) * time.Millisecond)
			}
		}()
	}
	wg.Wait()

	if _, total := h.collect(); total != goroutines*perGoroutine {
		t.Fatalf("total=%d, want %d", total, goroutines*perGoroutine)
	}
}

func TestSendHeartbeatRecordsRTT(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, len(pingPayload))
		if _, err := io.ReadFull(server, buf); err != nil {
			done <- err
			return
		}
		_, err := server.Write(buf)
		done <- err
	}()

	stats := &Stats{rtt: newRttHistogram()}
	ping := []byte(pingPayload)
	response := make([]byte, len(ping))
	if err := sendHeartbeat(client, validClientConfig(), ping, response, stats); err != nil {
		t.Fatalf("sendHeartbeat failed: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("server side failed: %v", err)
	}
	if _, total := stats.rtt.collect(); total != 1 {
		t.Fatalf("expected 1 RTT sample, got %d", total)
	}
}

func TestScheduleSlotStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ready := make(chan *Slot, 1)
	scheduleSlot(ctx, &Slot{id: 1}, 0, ready)

	select {
	case <-ready:
		t.Fatalf("slot scheduled after context cancellation")
	default:
	}
}

func TestScheduleSlotDelayedDelivery(t *testing.T) {
	ready := make(chan *Slot, 1)
	scheduleSlot(context.Background(), &Slot{id: 7}, 5*time.Millisecond, ready)

	select {
	case s := <-ready:
		if s.id != 7 {
			t.Fatalf("delivered slot %d, want 7", s.id)
		}
	case <-time.After(time.Second):
		t.Fatalf("slot not delivered after delay")
	}
}

func TestMaintainReportsIntervalSurvival(t *testing.T) {
	tests := []struct {
		name     string
		echoes   int
		survived bool
	}{
		{"dies before any interval heartbeat", 1, false},
		{"survives one interval heartbeat", 2, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()

			go func() {
				defer server.Close()
				buf := make([]byte, len(pingPayload))
				for i := 0; i < tt.echoes; i++ {
					if _, err := io.ReadFull(server, buf); err != nil {
						return
					}
					if _, err := server.Write(buf); err != nil {
						return
					}
				}
			}()

			cfg := validClientConfig()
			cfg.heartbeat = 20 * time.Millisecond
			ping := []byte(pingPayload)
			response := make([]byte, len(ping))
			stats := &Stats{}
			survived, err := maintain(context.Background(), client, cfg, ping, response, stats)
			if err == nil {
				t.Fatalf("maintain returned nil error on a closed pipe")
			}
			if survived != tt.survived {
				t.Fatalf("survived = %v, want %v", survived, tt.survived)
			}
			if got, want := stats.snapshot().intervalAcks, uint64(tt.echoes-1); got != want {
				t.Fatalf("intervalAcks = %d, want %d", got, want)
			}
		})
	}
}

func TestRateLimiterDeliversToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rl := newRateLimiter(ctx, 100)
	waitCtx, waitCancel := context.WithTimeout(ctx, 2*time.Second)
	defer waitCancel()
	if !rl.Wait(waitCtx) {
		t.Fatalf("no token delivered within 2s at rate 100/s")
	}
}

func TestRateLimiterWaitReturnsFalseOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rl := newRateLimiter(ctx, 1)
	cancel()

	if rl.Wait(ctx) {
		t.Fatalf("Wait returned true on a canceled context")
	}
}

func TestDialLoggerDedup(t *testing.T) {
	l := newDialLogger()
	t0 := time.Now()

	if !l.shouldLog("refused", t0) {
		t.Fatalf("first occurrence must log")
	}
	if l.shouldLog("refused", t0.Add(dialLogWindow/2)) {
		t.Fatalf("repeat within window must not log")
	}
	if !l.shouldLog("timeout", t0.Add(time.Millisecond)) {
		t.Fatalf("distinct error must log")
	}
	if !l.shouldLog("refused", t0.Add(dialLogWindow+time.Millisecond)) {
		t.Fatalf("repeat after window must log again")
	}

	var nilLogger *dialLogger
	if nilLogger.shouldLog("x", t0) {
		t.Fatalf("nil logger must not log")
	}
}

func TestRefillIssuesExactRate(t *testing.T) {
	const ticksPerSecond = 100
	for _, rate := range []int{1, 7, 99, 100, 150, maxStartRate} {
		for _, seconds := range []int{1, 3} {
			carry, total := 0, 0
			for i := 0; i < seconds*ticksPerSecond; i++ {
				var n int
				n, carry = refill(carry, rate, ticksPerSecond)
				total += n
				if carry < 0 || carry >= ticksPerSecond {
					t.Fatalf("rate %d: carry %d escaped [0, %d)", rate, carry, ticksPerSecond)
				}
			}
			if total != rate*seconds {
				t.Errorf("rate %d over %ds issued %d tokens, want %d", rate, seconds, total, rate*seconds)
			}
		}
	}
}

func TestBucketCapacityIsTenthOfRate(t *testing.T) {
	for rate, want := range map[int]int{1: 1, 9: 1, 10: 1, 100: 10, 1000: 100, maxStartRate: maxStartRate / 10} {
		if got := bucketCapacity(rate); got != want {
			t.Errorf("bucketCapacity(%d) = %d, want %d", rate, got, want)
		}
	}
}
