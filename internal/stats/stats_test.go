package stats

import (
	"math"
	"testing"
	"time"
)

func TestSufficient(t *testing.T) {
	for _, tt := range []struct {
		n        uint64
		perMille int
		want     bool
	}{
		{1, 500, false}, {2, 500, true},
		{19, 950, false}, {20, 950, true},
		{99, 990, false}, {100, 990, true},
		{999, 999, false}, {1000, 999, true},
	} {
		if got := Sufficient(tt.n, tt.perMille); got != tt.want {
			t.Errorf("Sufficient(%d, %d) = %v", tt.n, tt.perMille, got)
		}
	}
}

func TestQuantileIsNearestRank(t *testing.T) {
	var s []time.Duration
	for i := 100; i >= 1; i-- {
		s = append(s, time.Duration(i)*time.Millisecond)
	}
	for _, tt := range []struct {
		perMille int
		want     time.Duration
	}{{500, 50 * time.Millisecond}, {950, 95 * time.Millisecond}, {990, 99 * time.Millisecond}, {999, 100 * time.Millisecond}} {
		if got := Quantile(s, tt.perMille); got != tt.want {
			t.Errorf("Quantile(1..100 ms, %d) = %v, want %v", tt.perMille, got, tt.want)
		}
	}
	if got := Quantile([]time.Duration{7, 3}, 500); got != 3 {
		t.Errorf("p50 of two = %v, want the lower", got)
	}
}

// Reference values: the WP5 contract's sampling table (Wilson 95%).
func TestWilson(t *testing.T) {
	for _, tt := range []struct {
		k, n   uint64
		lo, hi float64
	}{
		{3, 300, 0.0034, 0.0290},
		{10, 1000, 0.0054, 0.0183},
		{0, 1000, 0, 0.0038},
		{1000, 1000, 0.9962, 1},
	} {
		lo, hi := Wilson(tt.k, tt.n)
		if math.Abs(lo-tt.lo) > 0.0001 || math.Abs(hi-tt.hi) > 0.0001 {
			t.Errorf("Wilson(%d, %d) = [%.4f, %.4f], want [%.4f, %.4f]", tt.k, tt.n, lo, hi, tt.lo, tt.hi)
		}
	}
	if lo, hi := Wilson(0, 0); lo != 0 || hi != 1 {
		t.Errorf("Wilson(0, 0) = [%v, %v]", lo, hi)
	}
}
