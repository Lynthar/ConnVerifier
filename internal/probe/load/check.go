package load

import (
	"math"
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

// beyond reports whether more than 1% of n exceeded a validity limit.
func beyond(k, n uint64) bool { return k*100 > n }

// lagging reports whether more than 5% of a 10 ms timer's n wake-ups were late.
// Calibrated in a virtual machine: unthrottled runs reached 1%, a host held to a
// tenth of a core 11% and more in every phase.
func lagging(late, n uint64) bool { return late*20 > n }

// quantile is the metric for the nearest-rank quantile at perMille of d, or an
// insufficient one.
func quantile(id string, d []time.Duration, perMille int) result.Metric {
	n := uint64(len(d))
	if !stats.Sufficient(n, perMille) {
		return result.Metric{ID: id, Unit: result.UnitMs, Samples: n, Insufficient: true}
	}
	s := slices.Clone(d)
	return result.Millis(id, stats.Quantile(s, perMille), n)
}

// increase is loaded − idle at perMille, when both are known.
func increase(id string, loaded []time.Duration, idleMs float64, perMille int) (result.Metric, bool) {
	n := uint64(len(loaded))
	if idleMs <= 0 || !stats.Sufficient(n, perMille) {
		return result.Metric{}, false
	}
	idle := time.Duration(math.Round(idleMs*1000)) * time.Microsecond
	return result.Millis(id, stats.Quantile(slices.Clone(loaded), perMille)-idle, n), true
}

// windowed is the part of a probe stream sent in [from, to], resolved against tmax.
func windowed(s *echo.Stream, end time.Time, from, to time.Time, slip time.Duration) echo.Outcome {
	var probes []echo.Probe
	index := make(map[int]int)
	for i, p := range s.Probes {
		if !p.Sent.Before(from) && !p.Sent.After(to) {
			index[i] = len(probes)
			probes = append(probes, p)
		}
	}
	var arrivals []int
	for _, i := range s.Arrivals {
		if j, ok := index[i]; ok {
			arrivals = append(arrivals, j)
		}
	}
	return echo.Analyze(probes, arrivals, tmax, slip, end)
}

var probeQuantiles = []struct {
	id       string
	perMille int
}{{"p50", 500}, {"p95", 950}, {"p99", 990}}

// goodputIDs names a phase's goodput metrics in the order of its directions:
// download first, as runPhase opens them.
func goodputIDs(spec phaseSpec) []string {
	if spec.down && spec.up {
		return []string{spec.name + ".goodput.download", spec.name + ".goodput.upload"}
	}
	return []string{spec.name + ".goodput"}
}

// finish turns the phases and the probe streams into the tcp-load result.
func finish(c *result.Check, phases []phaseResult, ur *echo.UDPRun, tr *echo.TCPRun, notes []result.Message, counts *protocol.LoadCounts, base idle, cfg Config) {
	var lagTicks, lagOver, slipped, slipSent uint64
	var cpuMax float64
	for _, p := range phases {
		pre := p.spec.name + "."
		lagTicks += p.lagTicks
		lagOver += p.lagOver
		cpuMax = max(cpuMax, p.cpuBusy)
		measured := p.ran && p.stopped != stopError

		for i, id := range goodputIDs(p.spec) {
			if !measured {
				c.Metrics = append(c.Metrics, result.Metric{ID: id, Unit: result.UnitMbps, Insufficient: true})
				continue
			}
			m := result.Mbps(id, p.dirs[i].finalBPS)
			m.AtLeast = p.gpConf[i] != result.High
			c.Metrics = append(c.Metrics, m)
		}
		if !p.ran {
			continue
		}
		c.Metrics = append(c.Metrics,
			result.Level(pre+"goodput.confidence", slices.Min(p.gpConf)),
			result.Count(pre+"connections", uint64(p.conns)),
			result.Count(pre+"connect_failures", uint64(p.connFailed)),
			result.Bytes(pre+"bytes", uint64(p.bytes)))
		if p.waited > 0 {
			c.Metrics = append(c.Metrics, result.Millis(pre+"wait", p.waited, 0))
		}
		if !p.drained {
			c.NotProven = append(c.NotProven, msg("load.not_proven.queue_not_drained", map[string]any{"phase": p.spec.name, "wait_ms": p.waited.Milliseconds()}, pre+"wait"))
		}
		if p.foreignFailed > 0 {
			c.NotProven = append(c.NotProven, msg("load.not_proven.foreign_failed", map[string]any{"phase": p.spec.name, "count": p.foreignFailed}, pre+"foreign.failed"))
		}
		if p.rpm.ok && measured {
			c.Metrics = append(c.Metrics, result.RPM(pre+"rpm", p.rpm.rpm, p.rpm.samples))
		} else {
			c.Metrics = append(c.Metrics, result.Metric{ID: pre + "rpm", Unit: result.UnitRPM, Samples: p.rpm.samples, Insufficient: true})
		}
		c.Metrics = append(c.Metrics, result.Level(pre+"rpm.confidence", p.rpmConf))
		for _, kind := range []struct {
			id string
			d  []time.Duration
		}{{"self.", p.self}, {"foreign.", p.foreign}} {
			for _, q := range probeQuantiles[:2] {
				c.Metrics = append(c.Metrics, quantile(pre+kind.id+q.id, kind.d, q.perMille))
			}
		}
		c.Metrics = append(c.Metrics, result.Count(pre+"foreign.failed", uint64(p.foreignFailed)))

		from := p.settledAt
		if from.IsZero() {
			from = p.end.Add(-mad * cfg.interval)
		}
		if ur != nil {
			o := windowed(&ur.Stream, ur.End, from, p.end, cfg.slipLimit)
			slipped, slipSent = slipped+o.Slipped, slipSent+o.Sent
			for _, q := range probeQuantiles {
				c.Metrics = append(c.Metrics, quantile(pre+"udp.rtt."+q.id, o.RTT, q.perMille))
			}
			c.Metrics = append(c.Metrics, result.Ratio(pre+"udp.loss", o.Sent-o.Received, o.Sent))
			for _, x := range []struct {
				q      string
				pm     int
				idleMs float64
			}{{"p50", 500, base.udpP50}, {"p95", 950, base.udpP95}} {
				if m, ok := increase(pre+"udp.rtt."+x.q+".increase", o.RTT, x.idleMs, x.pm); ok {
					c.Metrics = append(c.Metrics, m)
				}
			}
		}
		if tr != nil {
			o := windowed(&tr.Stream, tr.End, from, p.end, cfg.slipLimit)
			slipped, slipSent = slipped+o.Slipped, slipSent+o.Sent
			for _, q := range probeQuantiles {
				c.Metrics = append(c.Metrics, quantile(pre+"tcp.rtt."+q.id, o.RTT, q.perMille))
			}
			for _, x := range []struct {
				q      string
				pm     int
				idleMs float64
			}{{"p50", 500, base.tcpP50}, {"p95", 950, base.tcpP95}} {
				if m, ok := increase(pre+"tcp.rtt."+x.q+".increase", o.RTT, x.idleMs, x.pm); ok {
					c.Metrics = append(c.Metrics, m)
				}
			}
		}
	}
	c.Metrics = append(c.Metrics,
		result.Count("lag.ticks", lagTicks),
		result.Count("lag.over_10ms", lagOver),
		result.Count("send_slip.over_10ms", slipped))
	if cpuMax > 0 {
		v := math.Round(cpuMax*1000) / 10
		c.Metrics = append(c.Metrics, result.Metric{ID: "cpu.busy_max", Unit: result.UnitPercent, Value: &v})
	}
	c.NotProven = append(c.NotProven, msg("load.not_proven.bottleneck_side", nil), msg("load.not_proven.host_traffic", nil))
	c.NotProven = append(c.NotProven, notes...)
	evaluate(c, phases, validity{slipped: slipped, slipSent: slipSent, node: counts, cpuMax: cfg.cpuMax})
}

// validity is what the status rules need beyond the phases themselves.
type validity struct {
	slipped, slipSent uint64               // independent probes sent late, of all sent
	node              *protocol.LoadCounts // nil when the session end went unanswered
	cpuMax            float64              // busy share of the available CPU at which this host counts as full
}

// evaluate applies the tcp-load status rules in order (method document, "Status
// rules"); TestStatusRules mirrors that table.
func evaluate(c *result.Check, phases []phaseResult, v validity) {
	ran, aborted := 0, 0
	for _, p := range phases {
		if p.ran {
			ran++
			if p.stopped == stopError {
				aborted++
			}
		}
	}
	switch {
	case ran == 0:
		c.Status = result.Invalid
		c.Warnings = append(c.Warnings, msg("load.warning.interrupted", nil))
		return
	case aborted == len(phases):
		c.Status = result.Error
		c.Error = &result.Message{Key: "load.error.all_aborted", Params: map[string]any{"error": phases[0].err.Error()}}
		return
	}

	var invalid []result.Message
	for _, p := range phases {
		if !p.ran {
			continue
		}
		if lagging(p.lagOver, p.lagTicks) {
			invalid = append(invalid, msg("load.warning.host_lag", map[string]any{"phase": p.spec.name, "count": p.lagOver, "ticks": p.lagTicks}, "lag.over_10ms"))
		}
		if p.cpuKnown && p.cpuBusy >= v.cpuMax {
			invalid = append(invalid, msg("load.warning.host_cpu", map[string]any{"phase": p.spec.name, "percent": math.Round(p.cpuBusy * 100)}, "cpu.busy_max"))
		}
		if p.capped && slices.Min(p.gpConf) != result.High {
			invalid = append(invalid, msg("load.warning.grant_connections", map[string]any{"phase": p.spec.name, "connections": p.conns}, p.spec.name+".connections"))
		}
	}
	if v.node != nil && lagging(v.node.LagOver, v.node.LagTicks) {
		invalid = append(invalid, msg("load.warning.node_lag", map[string]any{"count": v.node.LagOver, "ticks": v.node.LagTicks}))
	}
	if beyond(v.slipped, v.slipSent) {
		invalid = append(invalid, msg("baseline.warning.send_slip", map[string]any{"count": v.slipped}, "send_slip.over_10ms"))
	}
	if len(invalid) > 0 {
		c.Status = result.Invalid
		c.Warnings = append(c.Warnings, invalid...)
		return
	}

	c.Status = result.Pass
	warn := func(key string, params map[string]any, basis ...string) {
		c.Status = result.Warn
		c.Inferences = append(c.Inferences, msg(key, params, basis...))
	}
	for _, p := range phases {
		name := p.spec.name
		switch {
		case !p.ran || p.stopped == stopInterrupted:
			warn("load.inference.interrupted", map[string]any{"phase": name})
			continue
		case p.stopped == stopError:
			warn("load.inference.aborted", map[string]any{"phase": name, "error": p.err.Error()})
			continue
		}
		if p.connFailed > 0 {
			warn("load.inference.connect_failures", map[string]any{"phase": name, "count": p.connFailed, "timeout_ms": connectTimeout.Milliseconds()}, name+".connect_failures")
		}
		if slices.Min(p.gpConf) != result.High {
			warn("load.inference.not_saturated", map[string]any{"phase": name, "why": p.stopped}, name+".goodput.confidence")
		}
		if p.rpmConf != result.High {
			warn("load.inference.rpm_unsettled", map[string]any{"phase": name}, name+".rpm.confidence")
		}
		if p.rpm.ok && p.rpm.rpm < poorRPM {
			warn("load.inference.rpm_poor", map[string]any{"phase": name, "rpm": math.Round(p.rpm.rpm)}, name+".rpm")
		}
	}
}
