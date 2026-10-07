// Package stats holds the estimators every check shares, so a rule such as "when
// is a percentile reportable" exists once.
package stats

import (
	"math"
	"slices"
	"time"
)

// Sufficient reports whether n samples support the quantile at perMille (990 is
// p99): at least one sample must lie above it, n × (1000 − perMille) ≥ 1000. So
// p50 needs 2 samples, p95 20, p99 100 and p99.9 1000.
func Sufficient(n uint64, perMille int) bool {
	return n*uint64(1000-perMille) >= 1000
}

// Quantile returns the nearest-rank quantile at perMille of samples, which it
// sorts in place. The caller checks Sufficient first; with no samples it returns 0.
func Quantile(samples []time.Duration, perMille int) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	slices.Sort(samples)
	rank := (len(samples)*perMille + 999) / 1000
	return samples[max(rank, 1)-1]
}

// Wilson returns the 95% Wilson score interval for k successes in n trials, as
// fractions. With n = 0 it returns the uninformative [0, 1].
func Wilson(k, n uint64) (lo, hi float64) {
	if n == 0 {
		return 0, 1
	}
	const z = 1.959963984540054 // two-sided 95%
	p, fn := float64(k)/float64(n), float64(n)
	d := 1 + z*z/fn
	c := p + z*z/(2*fn)
	h := z * math.Sqrt(p*(1-p)/fn+z*z/(4*fn*fn))
	return max(0, (c-h)/d), min(1, (c+h)/d)
}
