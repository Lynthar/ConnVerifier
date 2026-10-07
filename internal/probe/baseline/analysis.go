package baseline

import (
	"math/rand/v2"
	"slices"
	"time"
)

// defaultSlipLimit is how late a packet may leave, or how long the node may hold
// one, before it counts against validity; over 1% of packets beyond it is INVALID.
const defaultSlipLimit = 10 * time.Millisecond

// schedule returns n send offsets drawn uniformly over d and sorted: for a fixed
// count this is equivalent to Poisson sampling (RFC 2330 §11.1.1).
func schedule(n int, d time.Duration, rng *rand.Rand) []time.Duration {
	s := make([]time.Duration, n)
	for i := range s {
		s[i] = time.Duration(rng.Int64N(int64(d)))
	}
	slices.Sort(s)
	return s
}

// probe is one echo request and what came back for it, in arrival order.
type probe struct {
	planned, sent time.Time
	replies       []reply
}

type reply struct {
	at        time.Time
	reflSeq   uint32        // the node's own count (STAMP only)
	residence time.Duration // T3 − T2 at the node (STAMP only)
}

// outcome is a stream of probes resolved against a waiting time.
type outcome struct {
	sent, received, late    uint64
	reordered, reorderedFwd uint64
	dupForward, dupReturn   uint64
	slipped, slowNode       uint64
	unresolved              uint64 // left out: tmax had not passed when observation ended
	rtt, residence          []time.Duration
}

// analyze resolves probes seen until end: received if the first reply came within
// tmax, else lost (late if one came after, RFC 6673 §4.4); a probe whose tmax had not
// passed is left out. arrivals holds probe indexes in first-reply order.
func analyze(probes []probe, arrivals []int, tmax, slipLimit time.Duration, end time.Time) outcome {
	var o outcome
	counted := make([]bool, len(probes))
	for i, p := range probes {
		deadline := p.sent.Add(tmax)
		first := len(p.replies) > 0
		inTime := first && !p.replies[0].at.After(deadline)
		if !inTime && deadline.After(end) {
			o.unresolved++
			continue
		}
		counted[i] = true
		o.sent++
		if p.sent.Sub(p.planned) > slipLimit {
			o.slipped++
		}
		if !inTime {
			if first {
				o.late++
			}
			continue
		}
		o.received++
		r := p.replies[0]
		o.rtt = append(o.rtt, r.at.Sub(p.sent))
		o.residence = append(o.residence, r.residence)
		if r.residence > slipLimit {
			o.slowNode++
		}
		for j, d := range p.replies[1:] {
			if slices.ContainsFunc(p.replies[:j+1], func(e reply) bool { return e.reflSeq == d.reflSeq }) {
				o.dupReturn++ // the same reflection twice: duplicated on the way back
			} else {
				o.dupForward++ // the node saw the packet twice
			}
		}
	}

	var order []int
	for _, i := range arrivals {
		if counted[i] && len(probes[i].replies) > 0 && !probes[i].replies[0].at.After(probes[i].sent.Add(tmax)) {
			order = append(order, i)
		}
	}
	o.reordered = reordered(order)
	slices.SortStableFunc(order, func(a, b int) int {
		return int(int64(probes[a].replies[0].reflSeq) - int64(probes[b].replies[0].reflSeq))
	})
	o.reorderedFwd = reordered(order)
	return o
}

// reordered counts packets arriving below the next expected sequence number,
// which never decreases (RFC 4737 §3.3).
func reordered(seqs []int) uint64 {
	var n uint64
	next := 0
	for _, s := range seqs {
		if s < next {
			n++
			continue
		}
		next = s + 1
	}
	return n
}
