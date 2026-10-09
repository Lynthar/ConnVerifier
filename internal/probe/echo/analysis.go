package echo

import (
	"math/rand/v2"
	"slices"
	"time"
)

// DefaultSlipLimit is how late a packet may leave, or how long the node may hold
// one, before it counts against validity; over 1% of packets beyond it is INVALID.
const DefaultSlipLimit = 10 * time.Millisecond

// Schedule returns n send offsets drawn uniformly over d and sorted: for a fixed
// count this is equivalent to Poisson sampling (RFC 2330 §11.1.1).
func Schedule(n int, d time.Duration, rng *rand.Rand) []time.Duration {
	s := make([]time.Duration, n)
	for i := range s {
		s[i] = time.Duration(rng.Int64N(int64(d)))
	}
	slices.Sort(s)
	return s
}

// Probe is one echo request and what came back for it, in arrival order.
type Probe struct {
	Planned, Sent time.Time
	Replies       []Reply
}

type Reply struct {
	At        time.Time
	ReflSeq   uint32        // the node's own count (STAMP only)
	Residence time.Duration // T3 − T2 at the node (STAMP only)
}

// Outcome is a stream of probes resolved against a waiting time.
type Outcome struct {
	Sent, Received, Late    uint64
	Reordered, ReorderedFwd uint64
	DupForward, DupReturn   uint64
	Slipped, SlowNode       uint64
	Unresolved              uint64 // left out: tmax had not passed when observation ended
	RTT, Residence          []time.Duration
}

// Analyze resolves probes seen until end: received if the first reply came within
// tmax, else lost (late if one came after, RFC 6673 §4.4); a probe whose tmax had not
// passed is left out. arrivals holds probe indexes in first-reply order.
func Analyze(probes []Probe, arrivals []int, tmax, slipLimit time.Duration, end time.Time) Outcome {
	var o Outcome
	counted := make([]bool, len(probes))
	for i, p := range probes {
		deadline := p.Sent.Add(tmax)
		first := len(p.Replies) > 0
		inTime := first && !p.Replies[0].At.After(deadline)
		if !inTime && deadline.After(end) {
			o.Unresolved++
			continue
		}
		counted[i] = true
		o.Sent++
		if p.Sent.Sub(p.Planned) > slipLimit {
			o.Slipped++
		}
		if !inTime {
			if first {
				o.Late++
			}
			continue
		}
		o.Received++
		r := p.Replies[0]
		o.RTT = append(o.RTT, r.At.Sub(p.Sent))
		o.Residence = append(o.Residence, r.Residence)
		if r.Residence > slipLimit {
			o.SlowNode++
		}
		for j, d := range p.Replies[1:] {
			if slices.ContainsFunc(p.Replies[:j+1], func(e Reply) bool { return e.ReflSeq == d.ReflSeq }) {
				o.DupReturn++ // the same reflection twice: duplicated on the way back
			} else {
				o.DupForward++ // the node saw the packet twice
			}
		}
	}

	var order []int
	for _, i := range arrivals {
		if counted[i] && len(probes[i].Replies) > 0 && !probes[i].Replies[0].At.After(probes[i].Sent.Add(tmax)) {
			order = append(order, i)
		}
	}
	o.Reordered = reordered(order)
	slices.SortStableFunc(order, func(a, b int) int {
		return int(int64(probes[a].Replies[0].ReflSeq) - int64(probes[b].Replies[0].ReflSeq))
	})
	o.ReorderedFwd = reordered(order)
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
