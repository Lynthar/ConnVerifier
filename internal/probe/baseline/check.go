package baseline

import (
	"slices"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/protocol"
	"github.com/Lynthar/ConnVerifier/internal/result"
	"github.com/Lynthar/ConnVerifier/internal/stats"
)

func msg(key string, params map[string]any, basis ...string) result.Message {
	return result.Message{Key: key, Params: params, Basis: basis}
}

// notRun marks a check that did not start because the node granted too little.
func notRun(c *result.Check, key string, params map[string]any) {
	c.Status = result.Invalid
	c.Warnings = append(c.Warnings, msg(key, params))
}

// interruptedBeforeData marks a check the user stopped before it measured
// anything: there is no data, but nothing failed either, so it is not ERROR.
func interruptedBeforeData(c *result.Check) {
	c.Status, c.Error = result.Invalid, nil
	c.Warnings = append(c.Warnings, msg("baseline.warning.interrupted", nil))
}

func metricValue(c result.Check, id string) float64 {
	for _, m := range c.Metrics {
		if m.ID == id && m.Value != nil {
			return *m.Value
		}
	}
	return 0
}

func unsupported(c *result.Check, key string) {
	c.Status = result.Unsupported
	c.Warnings = append(c.Warnings, msg(key, nil))
}

// beyond reports whether more than 1% of n exceeded a validity limit.
func beyond(k, n uint64) bool { return k*100 > n }

func sub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

var quantiles = []struct {
	id       string
	perMille int
}{{"p50", 500}, {"p95", 950}, {"p99", 990}, {"p99_9", 999}}

// delayMetrics reports the minimum RTT, its quantiles and the PDV quantiles
// (RTT − min, RFC 5481 §4.2) from raw samples.
func delayMetrics(rtt []time.Duration) []result.Metric {
	n := uint64(len(rtt))
	s := slices.Clone(rtt)
	slices.Sort(s)
	if n == 0 {
		return []result.Metric{{ID: "rtt.min", Unit: result.UnitMs, Insufficient: true}}
	}
	out := []result.Metric{result.Millis("rtt.min", s[0], n)}
	var pdv []result.Metric
	for _, q := range quantiles {
		if !stats.Sufficient(n, q.perMille) {
			out = append(out, result.Metric{ID: "rtt." + q.id, Unit: result.UnitMs, Samples: n, Insufficient: true})
			pdv = append(pdv, result.Metric{ID: "pdv." + q.id, Unit: result.UnitMs, Samples: n, Insufficient: true})
			continue
		}
		v := stats.Quantile(s, q.perMille)
		out = append(out, result.Millis("rtt."+q.id, v, n))
		pdv = append(pdv, result.Millis("pdv."+q.id, v-s[0], n))
	}
	return append(out, pdv...)
}

// finishUDP turns a STAMP stream into the udp-baseline result. counts is what the
// node's reflector reported, or nil when the session end went unanswered.
func finishUDP(c *result.Check, r *udpRun, counts *protocol.StampCounts, tmax, slipLimit time.Duration) {
	o := analyze(r.probes, r.arrivals, tmax, slipLimit, r.end)
	lost := o.sent - o.received
	m := []result.Metric{
		result.Count("sent", o.sent),
		result.Count("replies", o.received),
		result.Count("lost", lost),
		result.Count("late", o.late),
		result.Ratio("loss", lost, o.sent),
	}
	var fwd, ret uint64
	if counts != nil {
		nodeGot := sub(counts.Received, o.dupForward) // each forward duplicate was answered too
		fwd, ret = sub(o.sent, nodeGot), sub(nodeGot, o.received)
		m = append(m,
			result.Count("lost.forward", fwd), result.Count("lost.return", ret),
			result.Ratio("loss.forward", fwd, o.sent), result.Ratio("loss.return", ret, nodeGot))
	}
	m = append(m,
		result.Count("duplicates.forward", o.dupForward),
		result.Count("duplicates.return", o.dupReturn),
		result.Ratio("duplication", o.dupForward+o.dupReturn, o.received),
		result.Count("reordered", o.reordered),
		result.Count("reordered.forward", o.reorderedFwd),
		result.Ratio("reordering", o.reordered, o.received))
	m = append(m, delayMetrics(o.rtt)...)
	res := slices.Clone(o.residence)
	slices.Sort(res)
	for _, q := range []struct {
		id string
		pm int
	}{{"node_residence.p50", 500}, {"node_residence.p99", 990}} {
		if stats.Sufficient(uint64(len(res)), q.pm) {
			m = append(m, result.Millis(q.id, stats.Quantile(res, q.pm), uint64(len(res))))
		} else {
			m = append(m, result.Metric{ID: q.id, Unit: result.UnitMs, Samples: uint64(len(res)), Insufficient: true})
		}
	}
	m = append(m, result.Count("send_slip.over_10ms", o.slipped), result.Count("send_errors", r.sendErrors))
	c.Metrics = m
	c.NotProven = append(c.NotProven, msg("baseline.not_proven.host_load", nil))
	if counts == nil {
		c.NotProven = append(c.NotProven, msg("udp_baseline.not_proven.direction_unknown", nil, "lost"))
	} else if fwd > 0 && ret > 0 {
		c.NotProven = append(c.NotProven, msg("udp_baseline.not_proven.return_after_forward_loss", nil, "lost.forward", "lost.return"))
	}
	if r.sendErrors > 0 {
		c.NotProven = append(c.NotProven, msg("udp_baseline.not_proven.send_errors", map[string]any{"count": r.sendErrors}, "send_errors"))
	}
	evaluateUDP(c, o, counts)
}

// evaluateUDP applies the udp-baseline status rules in order (method document,
// "Status rules"); TestUDPStatusRules mirrors that table.
func evaluateUDP(c *result.Check, o outcome, counts *protocol.StampCounts) {
	lost := o.sent - o.received
	switch {
	case o.sent == 0:
		c.Status = result.Error
		c.Error = &result.Message{Key: "udp_baseline.error.nothing_sent", Basis: []string{"sent", "send_errors"}}
		return
	case o.received == 0 && o.late == 0 && (counts == nil || counts.Received == 0):
		c.Status = result.Error
		key := "udp_baseline.error.no_udp_to_node"
		if counts == nil {
			key = "udp_baseline.error.no_reply"
		}
		c.Error = &result.Message{Key: key, Basis: []string{"replies"}}
		return
	case o.received == 0 && o.late == 0:
		c.Status = result.Warn
		c.Inferences = append(c.Inferences, msg("udp_baseline.inference.return_blocked", nil, "lost.return"))
		return
	}
	var invalid []result.Message
	if beyond(o.slipped, o.sent) {
		invalid = append(invalid, msg("baseline.warning.send_slip", map[string]any{"count": o.slipped}, "send_slip.over_10ms"))
	}
	if beyond(o.slowNode, o.received) {
		invalid = append(invalid, msg("udp_baseline.warning.node_slow", map[string]any{"count": o.slowNode}, "node_residence.p99"))
	}
	if counts != nil && counts.OverRate > 0 {
		invalid = append(invalid, msg("udp_baseline.warning.over_rate", map[string]any{"count": counts.OverRate}))
	}
	if counts != nil && counts.OtherAddr > 0 {
		invalid = append(invalid, msg("udp_baseline.warning.address_changed", map[string]any{"count": counts.OtherAddr}))
	}
	if len(invalid) > 0 {
		c.Status = result.Invalid
		c.Warnings = append(c.Warnings, invalid...)
		return
	}
	c.Status = result.Pass
	if lost > 0 {
		c.Status = result.Warn
		c.Inferences = append(c.Inferences, msg("udp_baseline.inference.loss", map[string]any{"count": lost, "late": o.late}, "loss", "late"))
	}
	if o.dupForward+o.dupReturn > 0 {
		c.Status = result.Warn
		c.Inferences = append(c.Inferences, msg("udp_baseline.inference.duplicates", map[string]any{"forward": o.dupForward, "return": o.dupReturn}, "duplicates.forward", "duplicates.return"))
	}
	if o.reordered > 0 {
		c.Status = result.Warn
		c.Inferences = append(c.Inferences, msg("udp_baseline.inference.reordering", map[string]any{"count": o.reordered, "forward": o.reorderedFwd}, "reordered", "reordered.forward"))
	}
}

// finishTCP turns an echo stream into the tcp-baseline result.
func finishTCP(c *result.Check, r *tcpRun, tmax, slipLimit time.Duration) {
	o := analyze(r.probes, r.arrivals, tmax, slipLimit, r.end)
	c.Metrics = append([]result.Metric{
		result.Count("sent", o.sent),
		result.Count("replies", o.received),
		result.Count("stalls", o.sent-o.received),
	}, delayMetrics(o.rtt)...)
	c.Metrics = append(c.Metrics, result.Count("send_slip.over_10ms", o.slipped))
	c.NotProven = append(c.NotProven,
		msg("baseline.not_proven.host_load", nil),
		msg("tcp_baseline.not_proven.node_scheduling", nil, "rtt.p50"))
	evaluateTCP(c, o, r.broke)
}

// evaluateTCP applies the tcp-baseline status rules in order; TestTCPStatusRules
// mirrors the method document's table.
func evaluateTCP(c *result.Check, o outcome, broke error) {
	stalls := o.sent - o.received
	switch {
	case o.received == 0 && broke != nil:
		c.Status = result.Error
		c.Error = &result.Message{Key: "tcp_baseline.error.broke", Params: map[string]any{"error": broke.Error()}, Basis: []string{"replies"}}
		return
	case beyond(o.slipped, o.sent):
		c.Status = result.Invalid
		c.Warnings = append(c.Warnings, msg("baseline.warning.send_slip", map[string]any{"count": o.slipped}, "send_slip.over_10ms"))
		return
	}
	c.Status = result.Pass
	if broke != nil {
		c.Status = result.Warn
		c.Inferences = append(c.Inferences, msg("tcp_baseline.inference.broke", map[string]any{"error": broke.Error()}, "replies"))
	}
	if stalls > 0 {
		c.Status = result.Warn
		c.Inferences = append(c.Inferences, msg("tcp_baseline.inference.stalls", map[string]any{"count": stalls}, "stalls", "rtt.p99"))
	}
}

// tcpFailed records a connection the node did not admit, following tcp-capacity
// rule 1: refusals for the node's own limits are INVALID, everything else ERROR.
func tcpFailed(c *result.Check, f *dataFailure) {
	switch {
	case f.kind == "reject" && (f.reject.Reason == protocol.ReasonBusy || f.reject.Reason == protocol.ReasonQuota):
		c.Status = result.Invalid
		c.Warnings = append(c.Warnings, msg("tcp_baseline.warning.node_refused", map[string]any{"reason": f.reject.Reason.String()}))
	case f.kind == "reject":
		c.Status = result.Error
		c.Error = &result.Message{Key: "tcp_baseline.error.rejected", Params: map[string]any{"reason": f.reject.Reason.String()}}
	case f.kind == "dial":
		c.Status = result.Error
		c.Error = &result.Message{Key: "tcp_baseline.error.no_connection", Params: map[string]any{"error": f.err.Error()}}
	default:
		c.Status = result.Error
		c.Error = &result.Message{Key: "tcp_baseline.error.handshake_failed", Params: map[string]any{"error": f.err.Error()}}
	}
}
