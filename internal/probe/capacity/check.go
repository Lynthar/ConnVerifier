package capacity

import (
	"fmt"
	"strings"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/protocol"
	"github.com/Lynthar/ConnVerifier/internal/result"
	"github.com/Lynthar/ConnVerifier/internal/stats"
)

// runInfo is what the session added to the counters: what the node granted, and
// whether the session's length rather than -duration ended the run.
type runInfo struct {
	granted protocol.Limits
	nodeCut bool
}

// idleTooShort records a session whose granted idle period ends before a
// heartbeat and its reply could arrive: the node would close every connection
// itself, so the run does not start.
func idleTooShort(c *result.Check, g protocol.Limits, need time.Duration) {
	c.Status = result.Invalid
	c.Warnings = append(c.Warnings, result.Message{
		Key:    "tcp_capacity.warning.idle_too_short",
		Params: map[string]any{"idle_ms": g.IdleTimeoutS * 1000, "needed_ms": need.Milliseconds()},
	})
}

func finishCheck(c *result.Check, cfg Config, f finalStats, h *rttHistogram, ri runInfo) {
	s := f.snap
	c.Metrics = []result.Metric{
		result.Count("connects", s.connects),
		result.Count("dial_errors", s.dialErrors),
		result.Count("dial_errors.host_resource", s.dialErrorsHost),
		result.Count("handshake_errors", s.handshakeErrors),
		result.Count("rejected.busy", s.rejectedBusy),
		result.Count("rejected.quota", s.rejectedQuota),
		result.Count("rejected.auth", s.rejectedAuth),
		result.Count("rejected.other", s.rejectedOther),
		result.Count("drops", s.drops()),
		result.Count("drops.timeout", s.dropTimeouts),
		result.Count("drops.closed", s.dropClosed),
		result.Count("drops.error", s.dropErrors),
		result.Count("drops.bad_ack", s.dropBadAcks),
		result.Count("drops.node_closed", s.dropNodeClosed),
		result.Count("heartbeats.sent", s.heartbeatsSent),
		result.Count("heartbeats.acked", s.heartbeatsAck),
		result.Count("heartbeats.interval_acked", s.intervalAcks),
	}
	for _, p := range []int{50, 95, 99} {
		c.Metrics = append(c.Metrics, quantileMetric(h, f.rttCounts, f.rttTotal, p))
	}
	if drops := s.drops(); drops > 0 {
		c.Metrics = append(c.Metrics, result.Millis("dropped_session.mean", time.Duration(s.sessionNanos/drops), drops))
	}
	if s.host.fdLimit > 0 {
		c.Metrics = append(c.Metrics, result.Count("host.fd_limit", s.host.fdLimit))
	}
	if s.host.ephemeralPorts > 0 {
		c.Metrics = append(c.Metrics, result.Count("host.ephemeral_ports", uint64(s.host.ephemeralPorts)))
	}
	evaluate(c, s, cfg, ri)
}

// quantileMetric reports percentile p only when at least one sample lies above it,
// samples × (100−p) ≥ 100; with fewer it is marked insufficient and has no value.
func quantileMetric(h *rttHistogram, counts []uint64, total uint64, p int) result.Metric {
	id := fmt.Sprintf("echo_rtt.p%d", p)
	if !stats.Sufficient(total, p*10) {
		return result.Metric{ID: id, Unit: result.UnitMs, Samples: total, Insufficient: true}
	}
	return result.Millis(id, h.quantile(counts, total, float64(p)/100), total)
}

func msg(key string, params map[string]any, basis ...string) result.Message {
	return result.Message{Key: key, Params: params, Basis: basis}
}

// evaluate sets the run's status and messages from the snapshot alone, in rule
// order: no connection admitted, a bottleneck outside the network, then drops,
// limits the node set, and whether any connection survived a full interval.
func evaluate(c *result.Check, s snapshot, cfg Config, ri runInfo) {
	if s.connects == 0 {
		noneAdmitted(c, s, cfg, ri)
		return
	}
	if w := limitWarnings(s, cfg, ri); len(w) > 0 {
		c.Status = result.Invalid
		c.Warnings = append(c.Warnings, w...)
		return
	}

	c.Status = result.Pass
	if s.handshakeErrors > 0 {
		c.NotProven = append(c.NotProven, msg("tcp_capacity.not_proven.handshake_errors",
			map[string]any{"count": s.handshakeErrors}, "handshake_errors"))
	}
	if s.drops() > 0 {
		c.Status = result.Warn
		for _, b := range []struct {
			metric, key string
			n           uint64
		}{
			{"drops.timeout", "tcp_capacity.inference.silent_drops", s.dropTimeouts},
			{"drops.closed", "tcp_capacity.inference.peer_closed", s.dropClosed},
			{"drops.error", "tcp_capacity.inference.reset_or_error", s.dropErrors},
			{"drops.bad_ack", "tcp_capacity.inference.bad_ack", s.dropBadAcks},
		} {
			if b.n > 0 {
				c.Inferences = append(c.Inferences, msg(b.key, map[string]any{"count": b.n}, b.metric))
			}
		}
		if s.dropNodeClosed > 0 {
			c.Inferences = append(c.Inferences, msg("tcp_capacity.inference.node_closed",
				map[string]any{"count": s.dropNodeClosed, "reasons": nodeCloseReasons(s)}, "drops.node_closed"))
		}
		if s.dropTimeouts > 0 {
			c.NotProven = append(c.NotProven, msg("tcp_capacity.not_proven.lifetime_upper_bound",
				map[string]any{"heartbeat_ms": cfg.heartbeat.Milliseconds()}, "dropped_session.mean"))
		}
	}
	if ri.granted.DialRate < cfg.startRate {
		c.Status = result.Warn
		c.Warnings = append(c.Warnings, msg("tcp_capacity.warning.dial_rate_cut",
			map[string]any{"granted": ri.granted.DialRate, "asked": cfg.startRate}))
	}
	if ri.nodeCut {
		c.Status = result.Warn
		c.Warnings = append(c.Warnings, msg("tcp_capacity.warning.duration_cut",
			map[string]any{"granted_ms": ri.granted.DurationS * 1000}))
	}
	if s.intervalAcks == 0 {
		c.Status = result.Warn
		c.NotProven = append(c.NotProven, msg("tcp_capacity.not_proven.no_full_interval",
			map[string]any{"heartbeat_ms": cfg.heartbeat.Milliseconds()}, "heartbeats.interval_acked"))
	}
}

// noneAdmitted decides a run in which the node admitted no connection: a refused
// ticket or failing dials mean no measurement (ERROR); a node at its limits, or a
// run that ended first, leave data that must not be judged (INVALID).
func noneAdmitted(c *result.Check, s snapshot, cfg Config, ri runInfo) {
	errMsg := func(key string, params map[string]any, basis ...string) {
		c.Status = result.Error
		m := msg(key, params, basis...)
		c.Error = &m
	}
	switch {
	case s.rejectedAuth > 0:
		errMsg("tcp_capacity.error.rejected_auth", map[string]any{"count": s.rejectedAuth}, "rejected.auth")
	case s.dialErrors > 0 && s.dialErrorsHost == s.dialErrors:
		errMsg("tcp_capacity.error.no_connection_host_resource",
			map[string]any{"dial_errors": s.dialErrors, "last_error": s.lastError}, "dial_errors", "dial_errors.host_resource")
	case s.dialErrors > 0:
		errMsg("tcp_capacity.error.no_connection",
			map[string]any{"dial_errors": s.dialErrors, "last_error": s.lastError}, "connects", "dial_errors")
	case s.handshakeErrors > 0:
		errMsg("tcp_capacity.error.handshake_failed",
			map[string]any{"count": s.handshakeErrors, "last_error": s.lastError}, "handshake_errors")
	case s.rejectedBusy+s.rejectedQuota > 0:
		c.Status = result.Invalid
		c.Warnings = append(c.Warnings, limitWarnings(s, cfg, ri)...)
	default:
		c.Status = result.Invalid
		c.Warnings = append(c.Warnings, msg("tcp_capacity.warning.ended_before_connect", nil, "connects", "dial_errors"))
	}
}

// limitWarnings lists the ways something other than the network bounded the run:
// the tool host (descriptors, ports) or the node (refusals, a smaller grant).
func limitWarnings(s snapshot, cfg Config, ri runInfo) []result.Message {
	target := min(cfg.targetConnections, ri.granted.Connections)
	var w []result.Message
	if s.dialErrorsHost > 0 {
		w = append(w, msg("tcp_capacity.warning.host_resource", map[string]any{"count": s.dialErrorsHost}, "dial_errors.host_resource"))
	}
	if s.host.fdLimit > 0 && s.host.fdLimit < uint64(target)+fdReserve {
		w = append(w, msg("tcp_capacity.warning.fd_limit", map[string]any{"limit": s.host.fdLimit, "target": target}, "host.fd_limit"))
	}
	if s.host.ephemeralPorts > 0 && s.host.ephemeralPorts < target {
		w = append(w, msg("tcp_capacity.warning.port_range", map[string]any{"ports": s.host.ephemeralPorts, "target": target}, "host.ephemeral_ports"))
	}
	if s.rejectedBusy > 0 {
		w = append(w, msg("tcp_capacity.warning.node_busy", map[string]any{"count": s.rejectedBusy}, "rejected.busy"))
	}
	if s.rejectedQuota > 0 {
		w = append(w, msg("tcp_capacity.warning.node_quota", map[string]any{"count": s.rejectedQuota}, "rejected.quota"))
	}
	if ri.granted.Connections < cfg.targetConnections {
		w = append(w, msg("tcp_capacity.warning.granted_less",
			map[string]any{"granted": ri.granted.Connections, "asked": cfg.targetConnections}))
	}
	return w
}

func nodeCloseReasons(s snapshot) string {
	var parts []string
	for _, r := range []struct {
		name string
		n    uint64
	}{{"idle_timeout", s.nodeClosedIdle}, {"session_ended", s.nodeClosedSession}, {"other", s.nodeClosedOther}} {
		if r.n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", r.name, r.n))
		}
	}
	return strings.Join(parts, " ")
}
