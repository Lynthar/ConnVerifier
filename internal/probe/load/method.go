package load

import (
	"math"
	"slices"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/result"
)

// Parameters of draft-ietf-ippm-responsiveness-09 §5.2 that are not flags; the
// interval (ID) is. Changing any of them changes the method version.
const (
	mad            = 4    // moving-average distance, in intervals
	sdt            = 0.05 // standard-deviation tolerance
	trimKeep       = 0.95 // the trimmed mean keeps the best 95%
	inp, inc       = 1, 1 // load connections at the start, and added per interval
	mnp            = 16   // load connections per direction at most
	mps            = 20   // probe pairs per second at most (the draft says 100)
	ptc            = 0.05 // probes may use at most 5% of the goodput
	probePairBytes = 6000 // §5.3: about 5000 for a foreign probe and 1000 for a self probe
	probeHeadroom  = 8    // load connections kept back for foreign probes
	poorRPM        = 300  // §1.2: below this responsiveness is "poor"
)

// stable reports whether the last mad values vary by less than sdt of the last
// one (§5.4): their population standard deviation below sdt × the latest.
func stable(v []float64) bool {
	if len(v) < mad {
		return false
	}
	w := v[len(v)-mad:]
	var mean float64
	for _, x := range w {
		mean += x
	}
	mean /= mad
	var sq float64
	for _, x := range w {
		sq += (x - mean) * (x - mean)
	}
	return math.Sqrt(sq/mad) < sdt*w[mad-1]
}

// trimmedMean is the mean of the best trimKeep of d in milliseconds: the
// single-sided trimmed mean of §5.3.1. It sorts d in place.
func trimmedMean(d []time.Duration) (float64, bool) {
	if len(d) == 0 {
		return 0, false
	}
	slices.Sort(d)
	keep := int(math.Ceil(trimKeep * float64(len(d))))
	var sum time.Duration
	for _, x := range d[:keep] {
		sum += x
	}
	return float64(sum.Microseconds()) / 1000 / float64(keep), true
}

// foreignProbe is one probe on a new connection: TCP handshake, TLS handshake
// (one round trip in TLS 1.3, so not divided) and the HTTP exchange; over QUIC
// the handshake is one round trip for transport and TLS together.
type foreignProbe struct {
	done                 time.Time
	tcp, tls, quic, http time.Duration
}

// selfProbe is one probe multiplexed on a load connection.
type selfProbe struct {
	done time.Time
	http time.Duration
}

// responsiveness is §5.3.1.1 over the given probes: the mean of the foreign and
// the loaded round trips per minute. ok is false without probes of both kinds.
// Over QUIC the draft gives no formula; following §5.3.1.2, each measured part
// of the foreign probe weighs equally, so the handshake and the exchange count
// half each.
type responsiveness struct {
	rpm, foreign, loaded float64
	samples              uint64
	ok                   bool
}

func rpmOf(foreign []foreignProbe, self []selfProbe, quic bool) responsiveness {
	var tcp, tls, qh, fh, lh []time.Duration
	for _, p := range foreign {
		tcp, tls, qh, fh = append(tcp, p.tcp), append(tls, p.tls), append(qh, p.quic), append(fh, p.http)
	}
	for _, p := range self {
		lh = append(lh, p.http)
	}
	a, ok1 := trimmedMean(tcp)
	b, _ := trimmedMean(tls)
	q, _ := trimmedMean(qh)
	c, _ := trimmedMean(fh)
	l, ok2 := trimmedMean(lh)
	foreignRTT := (a + b + c) / 3
	if quic {
		foreignRTT = (q + c) / 2
	}
	if !ok1 || !ok2 || foreignRTT <= 0 || l <= 0 {
		return responsiveness{}
	}
	f, s := 60000/foreignRTT, 60000/l
	return responsiveness{rpm: (f + s) / 2, foreign: f, loaded: s, samples: uint64(len(foreign) + len(self)), ok: true}
}

// confidence follows §5.4.1: low before mad intervals of a stage have run,
// medium when they have but it did not settle, high when it settled.
func confidence(intervals int, settled bool) int {
	switch {
	case settled:
		return result.High
	case intervals >= mad:
		return result.Medium
	}
	return result.Low
}
