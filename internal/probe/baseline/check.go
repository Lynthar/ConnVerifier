package baseline

import (
	"slices"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/probe/echo"
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

// finishUDP turns a STAMP stream into the udp-baseline result. counts is what the
// node's reflector reported, or nil when the session end went unanswered.
func finishUDP(c *result.Check, r *echo.UDPRun, counts *protocol.StampCounts, tmax, slipLimit time.Duration) {
	o := echo.Analyze(r.Probes, r.Arrivals, tmax, slipLimit, r.End)
	lost := o.Sent - o.Received
	m := []result.Metric{
		result.Count("sent", o.Sent),
		result.Count("replies", o.Received),
		result.Count("lost", lost),
		result.Count("late", o.Late),
		result.Ratio("loss", lost, o.Sent),
	}
	var fwd, ret uint64
	if counts != nil {
		nodeGot := sub(counts.Received, o.DupForward) // each forward duplicate was answered too
		fwd, ret = sub(o.Sent, nodeGot), sub(nodeGot, o.Received)
		m = append(m,
			result.Count("lost.forward", fwd), result.Count("lost.return", ret),
			result.Ratio("loss.forward", fwd, o.Sent), result.Ratio("loss.return", ret, nodeGot))
	}
	m = append(m,
		result.Count("duplicates.forward", o.DupForward),
		result.Count("duplicates.return", o.DupReturn),
		result.Ratio("duplication", o.DupForward+o.DupReturn, o.Received),
		result.Count("reordered", o.Reordered),
		result.Count("reordered.forward", o.ReorderedFwd),
		result.Ratio("reordering", o.Reordered, o.Received))
	m = append(m, echo.DelayMetrics("", o.RTT)...)
	res := slices.Clone(o.Residence)
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
	m = append(m, result.Count("send_slip.over_10ms", o.Slipped), result.Count("send_errors", r.SendErrors))
	c.Metrics = m
	c.NotProven = append(c.NotProven, msg("baseline.not_proven.host_load", nil))
	if counts == nil {
		c.NotProven = append(c.NotProven, msg("udp_baseline.not_proven.direction_unknown", nil, "lost"))
	} else if fwd > 0 && ret > 0 {
		c.NotProven = append(c.NotProven, msg("udp_baseline.not_proven.return_after_forward_loss", nil, "lost.forward", "lost.return"))
	}
	if r.SendErrors > 0 {
		c.NotProven = append(c.NotProven, msg("udp_baseline.not_proven.send_errors", map[string]any{"count": r.SendErrors}, "send_errors"))
	}
	evaluateUDP(c, o, counts)
}

// evaluateUDP applies the udp-baseline status rules in order (method document,
// "Status rules"); TestUDPStatusRules mirrors that table.
func evaluateUDP(c *result.Check, o echo.Outcome, counts *protocol.StampCounts) {
	lost := o.Sent - o.Received
	switch {
	case o.Sent == 0:
		c.Status = result.Error
		c.Error = &result.Message{Key: "udp_baseline.error.nothing_sent", Basis: []string{"sent", "send_errors"}}
		return
	case o.Received == 0 && o.Late == 0 && (counts == nil || counts.Received == 0):
		c.Status = result.Error
		key := "udp_baseline.error.no_udp_to_node"
		if counts == nil {
			key = "udp_baseline.error.no_reply"
		}
		c.Error = &result.Message{Key: key, Basis: []string{"replies"}}
		return
	case o.Received == 0 && o.Late == 0:
		c.Status = result.Warn
		c.Inferences = append(c.Inferences, msg("udp_baseline.inference.return_blocked", nil, "lost.return"))
		return
	}
	var invalid []result.Message
	if beyond(o.Slipped, o.Sent) {
		invalid = append(invalid, msg("baseline.warning.send_slip", map[string]any{"count": o.Slipped}, "send_slip.over_10ms"))
	}
	if beyond(o.SlowNode, o.Received) {
		invalid = append(invalid, msg("udp_baseline.warning.node_slow", map[string]any{"count": o.SlowNode}, "node_residence.p99"))
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
		c.Inferences = append(c.Inferences, msg("udp_baseline.inference.loss", map[string]any{"count": lost, "late": o.Late}, "loss", "late"))
	}
	if o.DupForward+o.DupReturn > 0 {
		c.Status = result.Warn
		c.Inferences = append(c.Inferences, msg("udp_baseline.inference.duplicates", map[string]any{"forward": o.DupForward, "return": o.DupReturn}, "duplicates.forward", "duplicates.return"))
	}
	if o.Reordered > 0 {
		c.Status = result.Warn
		c.Inferences = append(c.Inferences, msg("udp_baseline.inference.reordering", map[string]any{"count": o.Reordered, "forward": o.ReorderedFwd}, "reordered", "reordered.forward"))
	}
}

// finishTCP turns an echo stream into the tcp-baseline result.
func finishTCP(c *result.Check, r *echo.TCPRun, tmax, slipLimit time.Duration) {
	o := echo.Analyze(r.Probes, r.Arrivals, tmax, slipLimit, r.End)
	c.Metrics = append([]result.Metric{
		result.Count("sent", o.Sent),
		result.Count("replies", o.Received),
		result.Count("stalls", o.Sent-o.Received),
	}, echo.DelayMetrics("", o.RTT)...)
	c.Metrics = append(c.Metrics, result.Count("send_slip.over_10ms", o.Slipped))
	c.NotProven = append(c.NotProven,
		msg("baseline.not_proven.host_load", nil),
		msg("tcp_baseline.not_proven.node_scheduling", nil, "rtt.p50"))
	evaluateTCP(c, o, r.Broke)
}

// evaluateTCP applies the tcp-baseline status rules in order; TestTCPStatusRules
// mirrors the method document's table.
func evaluateTCP(c *result.Check, o echo.Outcome, broke error) {
	stalls := o.Sent - o.Received
	switch {
	case o.Received == 0 && broke != nil:
		c.Status = result.Error
		c.Error = &result.Message{Key: "tcp_baseline.error.broke", Params: map[string]any{"error": broke.Error()}, Basis: []string{"replies"}}
		return
	case beyond(o.Slipped, o.Sent):
		c.Status = result.Invalid
		c.Warnings = append(c.Warnings, msg("baseline.warning.send_slip", map[string]any{"count": o.Slipped}, "send_slip.over_10ms"))
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
func tcpFailed(c *result.Check, f *echo.DataFailure) {
	switch {
	case f.Kind == "reject" && (f.Reject.Reason == protocol.ReasonBusy || f.Reject.Reason == protocol.ReasonQuota):
		c.Status = result.Invalid
		c.Warnings = append(c.Warnings, msg("tcp_baseline.warning.node_refused", map[string]any{"reason": f.Reject.Reason.String()}))
	case f.Kind == "reject":
		c.Status = result.Error
		c.Error = &result.Message{Key: "tcp_baseline.error.rejected", Params: map[string]any{"reason": f.Reject.Reason.String()}}
	case f.Kind == "dial":
		c.Status = result.Error
		c.Error = &result.Message{Key: "tcp_baseline.error.no_connection", Params: map[string]any{"error": f.Err.Error()}}
	default:
		c.Status = result.Error
		c.Error = &result.Message{Key: "tcp_baseline.error.handshake_failed", Params: map[string]any{"error": f.Err.Error()}}
	}
}
