package load

import (
	"math"
	"testing"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/result"
)

func TestStable(t *testing.T) {
	for _, tt := range []struct {
		name string
		v    []float64
		want bool
	}{
		{"too few", []float64{100, 100, 100}, false},
		{"flat", []float64{1, 100, 100, 100, 100}, true},
		{"within 5%", []float64{96, 100, 104, 100}, true}, // σ = 2.83
		{"still rising", []float64{70, 80, 90, 100}, false},
		{"only the last four count", []float64{10, 1000, 100, 100, 100, 100}, true},
	} {
		if got := stable(tt.v); got != tt.want {
			t.Errorf("%s: stable(%v) = %v", tt.name, tt.v, got)
		}
	}
}

func ms(v ...int) []time.Duration {
	var d []time.Duration
	for _, x := range v {
		d = append(d, time.Duration(x)*time.Millisecond)
	}
	return d
}

// The trimmed mean drops the worst 5%: of 20 samples it keeps 19.
func TestTrimmedMean(t *testing.T) {
	d := ms(10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 1000)
	if m, ok := trimmedMean(d); !ok || m != 10 {
		t.Fatalf("trimmedMean = %v, %v; want 10 with the outlier dropped", m, ok)
	}
	if m, _ := trimmedMean(ms(20)); m != 20 {
		t.Fatalf("one sample: %v", m)
	}
	if _, ok := trimmedMean(nil); ok {
		t.Fatal("no samples gave a mean")
	}
}

// §5.3.1.1 worked through: foreign round trips 30, 30 and 60 ms average 40 ms,
// 1500 RPM; loaded 100 ms, 600 RPM; responsiveness their mean, 1050.
func TestRPM(t *testing.T) {
	f := []foreignProbe{{tcp: 30 * time.Millisecond, tls: 30 * time.Millisecond, http: 60 * time.Millisecond}}
	s := []selfProbe{{http: 100 * time.Millisecond}}
	r := rpmOf(f, s, false)
	if !r.ok || math.Abs(r.foreign-1500) > 1e-9 || math.Abs(r.loaded-600) > 1e-9 || math.Abs(r.rpm-1050) > 1e-9 || r.samples != 2 {
		t.Fatalf("rpmOf = %+v", r)
	}
	if rpmOf(f, nil, false).ok || rpmOf(nil, s, false).ok {
		t.Fatal("RPM from one kind of probe only")
	}
}

// Over QUIC the handshake and the exchange weigh half each: 40 and 60 ms average
// 50 ms, 1200 RPM; with the same loaded 600, responsiveness 900.
func TestRPMQUIC(t *testing.T) {
	f := []foreignProbe{{quic: 40 * time.Millisecond, http: 60 * time.Millisecond}}
	s := []selfProbe{{http: 100 * time.Millisecond}}
	r := rpmOf(f, s, true)
	if !r.ok || math.Abs(r.foreign-1200) > 1e-9 || math.Abs(r.loaded-600) > 1e-9 || math.Abs(r.rpm-900) > 1e-9 {
		t.Fatalf("rpmOf = %+v", r)
	}
}

func TestConfidence(t *testing.T) {
	for _, tt := range []struct {
		intervals int
		settled   bool
		want      int
	}{{3, false, result.Low}, {4, false, result.Medium}, {9, true, result.High}} {
		if got := confidence(tt.intervals, tt.settled); got != tt.want {
			t.Errorf("confidence(%d, %v) = %d, want %d", tt.intervals, tt.settled, got, tt.want)
		}
	}
}

// The queue counts as drained when the median recent round trip is within 10 ms
// or 20% of idle, whichever is more, judged on at least three replies.
func TestDrainedTo(t *testing.T) {
	for _, tt := range []struct {
		recent []time.Duration
		ref    time.Duration
		want   bool
	}{
		{ms(30, 31, 32), 25 * time.Millisecond, true},  // within 10 ms
		{ms(30, 36, 40), 25 * time.Millisecond, false}, // median 36: 11 ms above
		{ms(115, 118, 500), 100 * time.Millisecond, true},
		{ms(115, 125, 130), 100 * time.Millisecond, false}, // 20% of 100 ms is 20
		{ms(20, 20), 25 * time.Millisecond, false},         // too few replies
	} {
		if got := drainedTo(tt.recent, tt.ref); got != tt.want {
			t.Errorf("drainedTo(%v, %v) = %v", tt.recent, tt.ref, got)
		}
	}
}
