package baseline

import (
	"math/rand/v2"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func at(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

// sentAt is a probe sent on schedule at ms.
func sentAt(ms int, replies ...reply) probe {
	return probe{planned: at(ms), sent: at(ms), replies: replies}
}

func back(ms int, reflSeq uint32) reply { return reply{at: at(ms), reflSeq: reflSeq} }

const tmax = 3 * time.Second

func TestAnalyzeCountsReceivedLostLateAndUnresolved(t *testing.T) {
	probes := []probe{
		sentAt(0, back(50, 0)),
		sentAt(100),                // lost
		sentAt(200, back(3300, 1)), // late: after its 3 s
		sentAt(9000),               // unresolved: its 3 s had not passed at the end
		sentAt(9500, back(9550, 2)),
	}
	o := analyze(probes, []int{0, 4, 2}, tmax, defaultSlipLimit, at(10_000))
	if o.sent != 4 || o.received != 2 || o.late != 1 || o.unresolved != 1 {
		t.Fatalf("sent %d received %d late %d unresolved %d; want 4 2 1 1", o.sent, o.received, o.late, o.unresolved)
	}
	if len(o.rtt) != 2 || o.rtt[0] != 50*time.Millisecond || o.rtt[1] != 50*time.Millisecond {
		t.Fatalf("rtt %v", o.rtt)
	}
}

// A copy with the reflection number of an earlier one was duplicated on the way
// back; a copy with a new number means the node itself saw the packet twice.
func TestAnalyzeAttributesDuplicates(t *testing.T) {
	probes := []probe{
		sentAt(0, back(50, 0), back(51, 0)),
		sentAt(100, back(150, 1), back(152, 2)),
	}
	o := analyze(probes, []int{0, 1}, tmax, defaultSlipLimit, at(10_000))
	if o.dupReturn != 1 || o.dupForward != 1 || o.received != 2 {
		t.Fatalf("return %d forward %d received %d", o.dupReturn, o.dupForward, o.received)
	}
}

// RFC 4737: a packet below the next expected number is reordered, and the next
// expected number never goes down. The node's own numbering gives the order in
// which it received them, so forward reordering is counted apart.
func TestAnalyzeCountsReordering(t *testing.T) {
	probes := []probe{
		sentAt(0, back(50, 0)),
		sentAt(10, back(70, 2)), // the node received it after probe 2
		sentAt(20, back(60, 1)),
		sentAt(30, back(80, 3)),
	}
	o := analyze(probes, []int{0, 2, 1, 3}, tmax, defaultSlipLimit, at(10_000))
	if o.reordered != 1 || o.reorderedFwd != 1 {
		t.Fatalf("round trip %d forward %d; want 1 and 1", o.reordered, o.reorderedFwd)
	}
	if got := reordered([]int{0, 3, 1, 2, 4}); got != 2 {
		t.Fatalf("reordered(0 3 1 2 4) = %d, want 2", got)
	}
}

func TestAnalyzeCountsSendSlipAndSlowNode(t *testing.T) {
	p := sentAt(0, reply{at: at(60), residence: 11 * time.Millisecond})
	p.sent = p.sent.Add(11 * time.Millisecond)
	o := analyze([]probe{p, sentAt(100, back(150, 1))}, []int{0, 1}, tmax, defaultSlipLimit, at(10_000))
	if o.slipped != 1 || o.slowNode != 1 {
		t.Fatalf("slipped %d slow %d", o.slipped, o.slowNode)
	}
}

func TestScheduleIsSortedAndInRange(t *testing.T) {
	s := schedule(1000, 20*time.Second, rand.New(rand.NewPCG(1, 2)))
	if len(s) != 1000 {
		t.Fatalf("%d offsets", len(s))
	}
	for i, d := range s {
		if d < 0 || d >= 20*time.Second || (i > 0 && d < s[i-1]) {
			t.Fatalf("offset %d = %v out of order or range", i, d)
		}
	}
}
