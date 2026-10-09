package echo

import (
	"math/rand/v2"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func at(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

// sentAt is a probe sent on schedule at ms.
func sentAt(ms int, replies ...Reply) Probe {
	return Probe{Planned: at(ms), Sent: at(ms), Replies: replies}
}

func back(ms int, reflSeq uint32) Reply { return Reply{At: at(ms), ReflSeq: reflSeq} }

const tmax = 3 * time.Second

func TestAnalyzeCountsReceivedLostLateAndUnresolved(t *testing.T) {
	probes := []Probe{
		sentAt(0, back(50, 0)),
		sentAt(100),                // lost
		sentAt(200, back(3300, 1)), // late: after its 3 s
		sentAt(9000),               // unresolved: its 3 s had not passed at the end
		sentAt(9500, back(9550, 2)),
	}
	o := Analyze(probes, []int{0, 4, 2}, tmax, DefaultSlipLimit, at(10_000))
	if o.Sent != 4 || o.Received != 2 || o.Late != 1 || o.Unresolved != 1 {
		t.Fatalf("sent %d received %d late %d unresolved %d; want 4 2 1 1", o.Sent, o.Received, o.Late, o.Unresolved)
	}
	if len(o.RTT) != 2 || o.RTT[0] != 50*time.Millisecond || o.RTT[1] != 50*time.Millisecond {
		t.Fatalf("rtt %v", o.RTT)
	}
}

// A copy with the reflection number of an earlier one was duplicated on the way
// back; a copy with a new number means the node itself saw the packet twice.
func TestAnalyzeAttributesDuplicates(t *testing.T) {
	probes := []Probe{
		sentAt(0, back(50, 0), back(51, 0)),
		sentAt(100, back(150, 1), back(152, 2)),
	}
	o := Analyze(probes, []int{0, 1}, tmax, DefaultSlipLimit, at(10_000))
	if o.DupReturn != 1 || o.DupForward != 1 || o.Received != 2 {
		t.Fatalf("return %d forward %d received %d", o.DupReturn, o.DupForward, o.Received)
	}
}

// RFC 4737: a packet below the next expected number is reordered, and the next
// expected number never goes down. The node's own numbering gives the order in
// which it received them, so forward reordering is counted apart.
func TestAnalyzeCountsReordering(t *testing.T) {
	probes := []Probe{
		sentAt(0, back(50, 0)),
		sentAt(10, back(70, 2)), // the node received it after probe 2
		sentAt(20, back(60, 1)),
		sentAt(30, back(80, 3)),
	}
	o := Analyze(probes, []int{0, 2, 1, 3}, tmax, DefaultSlipLimit, at(10_000))
	if o.Reordered != 1 || o.ReorderedFwd != 1 {
		t.Fatalf("round trip %d forward %d; want 1 and 1", o.Reordered, o.ReorderedFwd)
	}
	if got := reordered([]int{0, 3, 1, 2, 4}); got != 2 {
		t.Fatalf("reordered(0 3 1 2 4) = %d, want 2", got)
	}
}

func TestAnalyzeCountsSendSlipAndSlowNode(t *testing.T) {
	p := sentAt(0, Reply{At: at(60), Residence: 11 * time.Millisecond})
	p.Sent = p.Sent.Add(11 * time.Millisecond)
	o := Analyze([]Probe{p, sentAt(100, back(150, 1))}, []int{0, 1}, tmax, DefaultSlipLimit, at(10_000))
	if o.Slipped != 1 || o.SlowNode != 1 {
		t.Fatalf("slipped %d slow %d", o.Slipped, o.SlowNode)
	}
}

func TestScheduleIsSortedAndInRange(t *testing.T) {
	s := Schedule(1000, 20*time.Second, rand.New(rand.NewPCG(1, 2)))
	if len(s) != 1000 {
		t.Fatalf("%d offsets", len(s))
	}
	for i, d := range s {
		if d < 0 || d >= 20*time.Second || (i > 0 && d < s[i-1]) {
			t.Fatalf("offset %d = %v out of order or range", i, d)
		}
	}
}
