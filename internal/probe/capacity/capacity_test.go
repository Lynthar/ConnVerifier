package capacity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/protocol"
)

// testInvite is a syntactically valid invite for a documentation-range address.
var testInvite = protocol.Invite{
	Label: "test", Addrs: []string{"192.0.2.10:7443"}, Pin: [32]byte{1}, Token: [32]byte{2},
}.Encode()

func validClientConfig() Config {
	return Config{
		node:              testInvite,
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
	t.Setenv(InviteEnv, "")
	if err := validClientConfig().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	tests := []struct {
		name   string
		update func(*Config)
	}{
		{name: "no node", update: func(c *Config) { c.node = "" }},
		{name: "malformed invite", update: func(c *Config) { c.node = "cvi1_x" }},
		{name: "missing invite file", update: func(c *Config) { c.node = "@" + t.TempDir() + "/none" }},
		{name: "zero clients", update: func(c *Config) { c.targetConnections = 0 }},
		{name: "excessive clients", update: func(c *Config) { c.targetConnections = maxClients + 1 }},
		{name: "zero start rate", update: func(c *Config) { c.startRate = 0 }},
		{name: "excessive start rate", update: func(c *Config) { c.startRate = maxStartRate + 1 }},
		{name: "zero heartbeat", update: func(c *Config) { c.heartbeat = 0 }},
		{name: "zero dial timeout", update: func(c *Config) { c.dialTimeout = 0 }},
		{name: "zero io timeout", update: func(c *Config) { c.ioTimeout = 0 }},
		{name: "zero min backoff", update: func(c *Config) { c.minBackoff = 0 }},
		{name: "max backoff below min", update: func(c *Config) { c.maxBackoff = c.minBackoff - time.Millisecond }},
		{name: "heartbeat beyond any idle grant", update: func(c *Config) { c.heartbeat = time.Duration(protocol.MaxIdleTimeoutS) * time.Second }},
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

func TestInviteFromFileAndEnvironment(t *testing.T) {
	path := t.TempDir() + "/invite"
	if err := os.WriteFile(path, []byte(testInvite+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := validClientConfig()
	cfg.node = "@" + path
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invite from file: %v", err)
	}
	t.Setenv(InviteEnv, testInvite)
	cfg.node = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invite from environment: %v", err)
	}
}

func TestWantLeavesRoomForHeartbeatAndStop(t *testing.T) {
	cfg := validClientConfig()
	cfg.heartbeat, cfg.ioTimeout, cfg.duration = 90*time.Second, 5*time.Second, time.Minute
	w := cfg.want()
	if w.IdleTimeoutS < 190 {
		t.Fatalf("idle %ds does not cover twice a heartbeat plus its reply", w.IdleTimeoutS)
	}
	if w.DurationS < 70 {
		t.Fatalf("duration %ds leaves no room to stop before the session ends", w.DurationS)
	}
	cfg.duration = 0
	if cfg.want().DurationS != 0 {
		t.Fatal("an open-ended run must ask for as long as the node allows")
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
	for range 1000 {
		if got := jitter(d); got < d/2 || got > d {
			t.Fatalf("jitter(%s) = %s, want within [%s, %s]", d, got, d/2, d)
		}
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
		{"other", errors.New("connection reset by peer"), func(s *Stats) uint64 { return s.dropErrors }},
		{"malformed frame", protocol.ErrMalformed, func(s *Stats) uint64 { return s.dropErrors }},
		{"bad ack", errBadAck, func(s *Stats) uint64 { return s.badAcks }},
		{"node closed", &nodeClosedError{protocol.ReasonIdleTimeout}, func(s *Stats) uint64 { return s.nodeClosedIdle }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stats := &Stats{}
			recordDropReason(tt.err, stats)
			if got := tt.want(stats); got != 1 {
				t.Fatalf("expected reason bucket = 1, got %d", got)
			}
			if total := stats.snapshot().drops(); total != 1 {
				t.Fatalf("expected exactly one drop, got %d", total)
			}
		})
	}
}

// The drop total is derived from the buckets, so it equals their sum by
// construction; this pins that no drop kind bypasses the buckets.
func TestSnapshotDropsEqualBuckets(t *testing.T) {
	stats := &Stats{}
	for _, err := range []error{
		fakeTimeoutError{}, io.EOF, errors.New("connection reset by peer"), errBadAck,
		&nodeClosedError{protocol.ReasonSessionEnded}, &nodeClosedError{protocol.ReasonShuttingDown}, nil,
	} {
		recordDropReason(err, stats)
	}
	s := stats.snapshot()
	sum := s.dropTimeouts + s.dropClosed + s.dropErrors + s.dropBadAcks + s.dropNodeClosed
	if s.drops() != sum || sum != 7 {
		t.Fatalf("drops = %d, bucket sum = %d, want both 7", s.drops(), sum)
	}
	if s.nodeClosedSession+s.nodeClosedIdle+s.nodeClosedOther != s.dropNodeClosed {
		t.Fatal("node-closed reasons do not add up to the node-closed bucket")
	}
}

func TestRttHistogramQuantile(t *testing.T) {
	h := newRttHistogram()
	for range 95 {
		h.Record(10 * time.Millisecond)
	}
	for range 5 {
		h.Record(500 * time.Millisecond)
	}
	counts, total := h.collect()
	if total != 100 {
		t.Fatalf("total=%d, want 100", total)
	}
	// Buckets are ~10% wide and quantile reports the upper bound: [value, value*1.1).
	if p50 := h.quantile(counts, total, 0.50); p50 < 10*time.Millisecond || p50 > 11*time.Millisecond {
		t.Fatalf("p50=%s, want ~10ms", p50)
	}
	if p99 := h.quantile(counts, total, 0.99); p99 < 500*time.Millisecond || p99 > 550*time.Millisecond {
		t.Fatalf("p99=%s, want ~500ms", p99)
	}
	if _, total2 := h.collect(); total2 != 0 {
		t.Fatalf("collect did not reset, total=%d", total2)
	}
}

func TestRttHistogramNilSafe(t *testing.T) {
	var h *rttHistogram
	h.Record(time.Millisecond)
	counts, total := h.collect()
	if total != 0 || counts != nil || h.quantile(counts, total, 0.5) != 0 {
		t.Fatal("nil histogram must read as empty")
	}
}

func TestRttHistogramConcurrentRecord(t *testing.T) {
	h := newRttHistogram()
	const goroutines, perGoroutine = 8, 1000
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for i := range perGoroutine {
				h.Record(time.Duration(i%50) * time.Millisecond)
			}
		})
	}
	wg.Wait()
	if _, total := h.collect(); total != goroutines*perGoroutine {
		t.Fatalf("total=%d, want %d", total, goroutines*perGoroutine)
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

func TestRefillIssuesExactRate(t *testing.T) {
	const ticksPerSecond = 100
	for _, rate := range []int{1, 7, 99, 100, 150, maxStartRate} {
		for _, seconds := range []int{1, 3} {
			carry, total := 0, 0
			for range seconds * ticksPerSecond {
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

// frameNode plays an admitted node on the far end of a pipe: answer runs on each
// PING and may reply, close or stay silent.
func frameNode(t *testing.T, conn net.Conn, answer func(n int, ping protocol.Frame) bool) {
	t.Helper()
	go func() {
		defer conn.Close()
		var fr protocol.FrameReader
		for n := 1; ; n++ {
			f, err := fr.Next(conn)
			if err != nil {
				return
			}
			if f.Type == protocol.TypePing && !answer(n, f) {
				return
			}
		}
	}()
}

func pong(conn net.Conn, seq uint64) {
	protocol.WriteFrame(conn, protocol.Frame{Type: protocol.TypePong, Seq: seq})
}

func TestMaintainReportsIntervalSurvival(t *testing.T) {
	for _, echoes := range []int{1, 3} {
		client, node := net.Pipe()
		frameNode(t, node, func(n int, ping protocol.Frame) bool {
			if n > echoes {
				return false // close: the far side ends the connection
			}
			pong(node, ping.Seq)
			return true
		})
		cfg := validClientConfig()
		cfg.heartbeat = 20 * time.Millisecond
		stats := &Stats{rtt: newRttHistogram()}
		survived, err := maintain(context.Background(), client, cfg, stats)
		client.Close()
		if !errors.Is(err, io.EOF) {
			t.Fatalf("echoes=%d: err = %v, want EOF from the close", echoes, err)
		}
		if want := echoes > 1; survived != want {
			t.Fatalf("echoes=%d: survived = %v, want %v", echoes, survived, want)
		}
		if got := stats.snapshot().intervalAcks; got != uint64(echoes-1) {
			t.Fatalf("echoes=%d: intervalAcks = %d, want %d", echoes, got, echoes-1)
		}
	}
}

// A CLOSE between heartbeats is seen when it arrives, not at the next heartbeat.
func TestMaintainSeesNodeCloseAtOnce(t *testing.T) {
	client, node := net.Pipe()
	frameNode(t, node, func(n int, ping protocol.Frame) bool {
		pong(node, ping.Seq)
		time.Sleep(50 * time.Millisecond)
		protocol.WriteFrame(node, protocol.Frame{Type: protocol.TypeClose, Reason: protocol.ReasonShuttingDown})
		return true
	})
	cfg := validClientConfig()
	cfg.heartbeat = 10 * time.Second
	start := time.Now()
	_, err := maintain(context.Background(), client, cfg, &Stats{rtt: newRttHistogram()})
	client.Close()
	var nc *nodeClosedError
	if !errors.As(err, &nc) || nc.reason != protocol.ReasonShuttingDown {
		t.Fatalf("err = %v, want a shutting_down close", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("close noticed after %s with a 10s heartbeat", elapsed)
	}
}

func TestMaintainBadAck(t *testing.T) {
	client, node := net.Pipe()
	frameNode(t, node, func(_ int, ping protocol.Frame) bool { pong(node, ping.Seq+1); return true })
	_, err := maintain(context.Background(), client, validClientConfig(), &Stats{rtt: newRttHistogram()})
	client.Close()
	if !errors.Is(err, errBadAck) {
		t.Fatalf("err = %v, want errBadAck", err)
	}
}

func TestMaintainSilentDropTimesOut(t *testing.T) {
	client, node := net.Pipe()
	defer node.Close()
	frameNode(t, node, func(n int, ping protocol.Frame) bool {
		if n == 1 {
			pong(node, ping.Seq)
		}
		return true // stays open but answers nothing more
	})
	cfg := validClientConfig()
	cfg.heartbeat, cfg.ioTimeout = 20*time.Millisecond, 50*time.Millisecond
	_, err := maintain(context.Background(), client, cfg, &Stats{rtt: newRttHistogram()})
	client.Close()
	if !isTimeout(err) {
		t.Fatalf("err = %v, want a timeout", err)
	}
}

func TestMaintainAnswersProbeAndStopsOnCancel(t *testing.T) {
	client, node := net.Pipe()
	defer node.Close()
	acked := make(chan uint64, 1)
	go func() {
		var fr protocol.FrameReader
		if f, err := fr.Next(node); err == nil && f.Type == protocol.TypePing {
			pong(node, f.Seq)
		}
		protocol.WriteFrame(node, protocol.Frame{Type: protocol.TypeProbe, Seq: 99})
		for {
			f, err := fr.Next(node)
			if err != nil {
				return
			}
			if f.Type == protocol.TypeProbeAck {
				acked <- f.Seq
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		cfg := validClientConfig()
		cfg.heartbeat = time.Hour
		_, err := maintain(ctx, client, cfg, &Stats{rtt: newRttHistogram()})
		done <- err
	}()
	select {
	case seq := <-acked:
		if seq != 99 {
			t.Fatalf("probe ack seq %d, want 99", seq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("probe not acknowledged")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("after cancel err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("maintain did not stop on cancel")
	}
	client.Close()
}

// A cancel that lands between reads must not be overwritten by the next read deadline.
func TestMaintainStopsWhenCancelledBetweenReads(t *testing.T) {
	client, node := net.Pipe()
	defer node.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		cfg := validClientConfig()
		cfg.heartbeat, cfg.ioTimeout = time.Hour, time.Hour
		_, err := maintain(ctx, client, cfg, &Stats{rtt: newRttHistogram()})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond) // hold the PING write until the cancel has set its deadline
	go io.Copy(io.Discard, node)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("maintain missed a cancel that landed between reads")
	}
	client.Close()
}
