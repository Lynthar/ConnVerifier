package echo

import (
	"slices"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/result"
	"github.com/Lynthar/ConnVerifier/internal/stats"
)

var quantiles = []struct {
	id       string
	perMille int
}{{"p50", 500}, {"p95", 950}, {"p99", 990}, {"p99_9", 999}}

// DelayMetrics reports the minimum RTT, its quantiles and the PDV quantiles
// (RTT − min, RFC 5481 §4.2) from raw samples, with prefix before each ID.
func DelayMetrics(prefix string, rtt []time.Duration) []result.Metric {
	n := uint64(len(rtt))
	s := slices.Clone(rtt)
	slices.Sort(s)
	if n == 0 {
		return []result.Metric{{ID: prefix + "rtt.min", Unit: result.UnitMs, Insufficient: true}}
	}
	out := []result.Metric{result.Millis(prefix+"rtt.min", s[0], n)}
	var pdv []result.Metric
	for _, q := range quantiles {
		if !stats.Sufficient(n, q.perMille) {
			out = append(out, result.Metric{ID: prefix + "rtt." + q.id, Unit: result.UnitMs, Samples: n, Insufficient: true})
			pdv = append(pdv, result.Metric{ID: prefix + "pdv." + q.id, Unit: result.UnitMs, Samples: n, Insufficient: true})
			continue
		}
		v := stats.Quantile(s, q.perMille)
		out = append(out, result.Millis(prefix+"rtt."+q.id, v, n))
		pdv = append(pdv, result.Millis(prefix+"pdv."+q.id, v-s[0], n))
	}
	return append(out, pdv...)
}
