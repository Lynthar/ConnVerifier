package load

import (
	"errors"
	"slices"
	"testing"

	"github.com/Lynthar/ConnVerifier/internal/protocol"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

// settled is a phase that ran to a settled goodput and responsiveness.
func settled(spec phaseSpec) phaseResult {
	r := phaseResult{spec: spec, ran: true, drained: true, stopped: stopSettled, rpmConf: result.High, rpm: responsiveness{rpm: 2000, ok: true}, lagTicks: 1000, cpuKnown: true, cpuBusy: 0.2}
	for range goodputIDs(spec) {
		r.gpConf = append(r.gpConf, result.High)
		r.unsteady = append(r.unsteady, false)
	}
	return r
}

func clean() []phaseResult {
	return []phaseResult{settled(phases[0]), settled(phases[1]), settled(phases[2])}
}

// with returns clean phases with the upload phase changed by f.
func with(f func(*phaseResult)) []phaseResult {
	p := clean()
	f(&p[1])
	return p
}

// quicLoads are clean quic-load phases whose download directions (down) or upload
// directions moved mbps with sender loss lossPct and the others as TCP did, against
// a tcp-load reference of 100 Mbit/s at tcpLoss percent in every direction.
func quicLoads(down bool, mbps, lossPct, tcpLoss float64) ([]phaseResult, validity) {
	p := clean()
	for i := range p {
		for _, d := range []bool{true, false} {
			if (d && !p[i].spec.down) || (!d && !p[i].spec.up) {
				continue
			}
			bps, loss := 100e6, tcpLoss
			if d == down {
				bps, loss = mbps*1e6, lossPct
			}
			p[i].dirs = append(p[i].dirs, &direction{down: d, finalBPS: bps, sent: 100_000, resent: uint64(loss * 1000), sentKnown: true})
		}
	}
	ref := map[string]tcpRef{}
	for _, spec := range phases {
		for _, id := range goodputIDs(spec) {
			ref[id] = tcpRef{bps: 100e6, loss: tcpLoss, sent: 100_000, lossKnown: true}
		}
	}
	return p, validity{slipSent: 1000, node: &protocol.LoadCounts{LagTicks: 1000}, cpuMax: 0.875, quic: true, tcp: ref}
}

// quicUploads is quicLoads for the uploads against TCP's 0.5%.
func quicUploads(mbps, lossPct float64) ([]phaseResult, validity) {
	return quicLoads(false, mbps, lossPct, 0.5)
}

// bidirectionalHeld is quicLoads(true, 60, 5, 0.5) with the download phase as TCP's:
// only the download of both directions at once meets the rule.
func bidirectionalHeld() []phaseResult {
	p, _ := quicLoads(true, 60, 5, 0.5)
	p[0].dirs[0] = &direction{down: true, finalBPS: 100e6, sent: 100_000, resent: 500, sentKnown: true}
	return p
}

func TestHeldDirectionBoundsBothAtOnce(t *testing.T) {
	for _, tt := range []struct {
		name   string
		phases []phaseResult
		want   []bool // bounded: download, upload of both directions at once
	}{
		{"held alone", func() []phaseResult { p, _ := quicLoads(true, 60, 5, 0.5); return p }(), []bool{true, false}},
		{"held only with both at once", bidirectionalHeld(), []bool{false, false}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, v := quicLoads(true, 60, 5, 0.5)
			v.held = heldDirections(tt.phases, v.tcp)
			for i, want := range tt.want {
				if got := bounded(tt.phases[2], i, v); got != want {
					t.Errorf("direction %d bounded %v, want %v", i, got, want)
				}
			}
		})
	}
}

// Rows follow the "Status rules" tables of docs/methods/tcp-load.md and
// quic-load.md, in order; quic-load adds an inference on rule 1 and rule 3's QUIC row.
func TestStatusRules(t *testing.T) {
	broke := errors.New("connection reset")
	valid := validity{slipSent: 1000, node: &protocol.LoadCounts{LagTicks: 1000}, cpuMax: 0.875}
	for _, tt := range []struct {
		name   string
		phases []phaseResult
		v      validity
		status result.Status
		key    string
	}{
		{"0 interrupted before any phase", []phaseResult{{spec: phases[0]}, {spec: phases[1]}, {spec: phases[2]}}, valid, result.Invalid, "load.warning.interrupted"},
		{"1 every phase broke", func() []phaseResult {
			p := clean()
			for i := range p {
				p[i].stopped, p[i].err = stopError, broke
			}
			return p
		}(), valid, result.Error, "load.error.all_aborted"},
		{"1 no QUIC handshake while UDP got through", func() []phaseResult {
			p := clean()
			for i := range p {
				p[i].stopped, p[i].err = stopError, broke
			}
			return p
		}(), validity{slipSent: 1000, cpuMax: 0.875, quicBlocked: true}, result.Error, "quic_load.inference.blocked"},
		{"2 the grant held the load", with(func(p *phaseResult) { p.capped, p.gpConf = true, []int{result.Medium} }), valid, result.Invalid, "load.warning.grant_connections"},
		{"3 a phase broke", with(func(p *phaseResult) { p.stopped, p.err = stopError, broke }), valid, result.Warn, "load.inference.aborted"},
		{"3 a phase was interrupted", with(func(p *phaseResult) { p.stopped = stopInterrupted }), valid, result.Warn, "load.inference.interrupted"},
		{"3 goodput not settled", with(func(p *phaseResult) { p.stopped, p.gpConf = stopBudget, []int{result.Low} }), valid, result.Warn, "load.inference.not_saturated"},
		{"3 goodput no longer settled at the end", with(func(p *phaseResult) { p.gpConf, p.unsteady = []int{result.Medium}, []bool{true} }), valid, result.Warn, "load.inference.goodput_unsteady"},
		{"3 responsiveness not settled", with(func(p *phaseResult) { p.stopped, p.rpmConf = stopTime, result.Medium }), valid, result.Warn, "load.inference.rpm_unsettled"},
		{"3 poor responsiveness", with(func(p *phaseResult) { p.rpm.rpm = 299 }), valid, result.Warn, "load.inference.rpm_poor"},
		{"3 load connections that did not open", with(func(p *phaseResult) { p.connFailed = 2 }), valid, result.Warn, "load.inference.connect_failures"},
		{"3 this host fell behind", with(func(p *phaseResult) { p.lagOver = 51 }), valid, result.Warn, "load.inference.host_limited"},
		{"3 this host fell behind, so latencies may read high", with(func(p *phaseResult) { p.lagOver = 51 }), valid, result.Warn, "load.inference.host_latency"},
		{"3 the probes were sent late", clean(), validity{slipped: 11, slipSent: 1000, cpuMax: 0.875}, result.Warn, "load.inference.host_latency"},
		{"3 the node fell behind", clean(), validity{slipSent: 1000, node: &protocol.LoadCounts{LagTicks: 1000, LagOver: 51}, cpuMax: 0.875}, result.Warn, "load.inference.node_limited"},
		{"3 something held QUIC, not TCP: ten times the loss", func() []phaseResult { p, _ := quicUploads(60, 5); return p }(), func() validity { _, v := quicUploads(60, 5); return v }(), result.Warn, "quic_load.inference.quic_held"},
		{"3 something held QUIC, not TCP: a tenth of the loss", func() []phaseResult { p, _ := quicUploads(60, 0.05); return p }(), func() validity { _, v := quicUploads(60, 0.05); return v }(), result.Warn, "quic_load.inference.quic_held"},
		{"4 clean", clean(), valid, result.Pass, ""},
		{"a full CPU is only noted", with(func(p *phaseResult) { p.cpuBusy = 0.9 }), valid, result.Pass, "load.not_proven.host_cpu"},
		{"QUIC at 0.7 of TCP is the path's", func() []phaseResult { p, _ := quicUploads(70, 5); return p }(), func() validity { _, v := quicUploads(70, 5); return v }(), result.Pass, ""},
		{"QUIC losing five times more is the path's", func() []phaseResult { p, _ := quicUploads(60, 2.5); return p }(), func() validity { _, v := quicUploads(60, 2.5); return v }(), result.Pass, ""},
		{"3 something held QUIC, not TCP: less loss", func() []phaseResult { p, _ := quicUploads(60, 0.4); return p }(), func() validity { _, v := quicUploads(60, 0.4); return v }(), result.Warn, "quic_load.inference.quic_held"},
		{"QUIC losing as much as TCP is the path's", func() []phaseResult { p, _ := quicUploads(60, 0.5); return p }(), func() validity { _, v := quicUploads(60, 0.5); return v }(), result.Pass, ""},
		{"3 something held a QUIC download, not TCP", func() []phaseResult { p, _ := quicLoads(true, 60, 5, 0.5); return p }(), func() validity { _, v := quicLoads(true, 60, 5, 0.5); return v }(), result.Warn, "quic_load.inference.quic_held"},
		{"3 something held QUIC, not TCP: six segments against lossless TCP", func() []phaseResult { p, _ := quicLoads(true, 60, 0.006, 0); return p }(), func() validity { _, v := quicLoads(true, 60, 0.006, 0); return v }(), result.Warn, "quic_load.inference.quic_held"},
		{"both directions at once are not judged on their own", bidirectionalHeld(), func() validity { _, v := quicLoads(true, 60, 5, 0.5); return v }(), result.Pass, ""},
		{"five segments against lossless TCP are noted", func() []phaseResult { p, _ := quicLoads(true, 60, 0.005, 0); return p }(), func() validity { _, v := quicLoads(true, 60, 0.005, 0); return v }(), result.Pass, "quic_load.not_proven.quic_held_tcp_lossless"},
		{"no TCP reference is noted", func() []phaseResult { p, _ := quicUploads(60, 5); return p }(), validity{slipSent: 1000, cpuMax: 0.875, quic: true, tcp: map[string]tcpRef{}}, result.Pass, "quic_load.not_proven.quic_held_no_tcp"},
		{"no sender counters is noted", func() []phaseResult {
			p, _ := quicUploads(60, 5)
			for i := range p {
				for _, d := range p[i].dirs {
					d.sentKnown = false
				}
			}
			return p
		}(), func() validity { _, v := quicUploads(60, 5); return v }(), result.Pass, "quic_load.not_proven.quic_held_no_counters"},
		{"a capped phase that settled is valid", with(func(p *phaseResult) { p.capped = true }), valid, result.Pass, ""},
		{"a capped phase that settled and then swung is valid", with(func(p *phaseResult) { p.capped, p.gpConf, p.unsteady = true, []int{result.Medium}, []bool{true} }), valid, result.Warn, "load.inference.goodput_unsteady"},
		{"5% late wake-ups are still valid", with(func(p *phaseResult) { p.lagOver = 50 }), valid, result.Pass, ""},
		{"1% late probes are still valid", clean(), validity{slipped: 10, slipSent: 1000, cpuMax: 0.875}, result.Pass, ""},
		{"300 RPM is not poor", with(func(p *phaseResult) { p.rpm.rpm = 300 }), valid, result.Pass, ""},
		{"an unanswered session end proves nothing", clean(), validity{slipSent: 1000, cpuMax: 0.875}, result.Pass, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var c result.Check
			evaluate(&c, tt.phases, tt.v)
			if c.Status != tt.status || (tt.key != "" && !slices.Contains(keys(c), tt.key)) {
				t.Fatalf("status %s messages %v; want %s with %s", c.Status, keys(c), tt.status, tt.key)
			}
		})
	}
}

// The latency note (quic-load section 5): QUIC below 0.7 of TCP's goodput whose UDP
// probe rose at least 3 times TCP's rise and 10 ms more.
func TestQueueOffPath(t *testing.T) {
	tcp := func(inc float64) tcpRef { return tcpRef{bps: 1000e6, udpInc: inc, incKnown: true} }
	for _, tc := range []struct {
		name  string
		mbps  float64
		incMs float64
		ref   tcpRef
		want  bool
	}{
		{"lighter load, deeper queue", 150, 40, tcp(1), true},
		{"TCP's rise below idle counts as none", 150, 10, tcp(-0.5), true},
		{"QUIC at 0.7 of TCP", 700, 40, tcp(1), false},
		{"under 3 times TCP's rise", 150, 29, tcp(10), false},
		{"under 10 ms more", 150, 9, tcp(0), false},
		{"TCP's rise unknown", 150, 40, tcpRef{bps: 1000e6}, false},
		{"no TCP", 150, 40, tcpRef{}, false},
	} {
		if got := queueOffPath(tc.mbps*1e6, tc.incMs, tc.ref); got != tc.want {
			t.Errorf("%s: queueOffPath = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The note names the phase and both loads' goodput and rise, and leaves the status alone.
func TestOffPathNote(t *testing.T) {
	p := settled(phases[0])
	p.dirs = []*direction{{down: true, finalBPS: 150e6}}
	ref := map[string]tcpRef{"download.goodput": {bps: 1000e6, udpInc: 1.04, incKnown: true}}
	var c result.Check
	offPathNote(&c, p, 40, ref)
	if len(c.NotProven) != 1 || c.Status != "" {
		t.Fatalf("notes %+v, status %q", c.NotProven, c.Status)
	}
	m := c.NotProven[0]
	want := map[string]any{"phase": "download", "quic_mbps": 150.0, "tcp_mbps": 1000.0, "quic_ms": 40.0, "tcp_ms": 1.0}
	if m.Key != "quic_load.not_proven.queue_off_path" || len(m.Params) != len(want) {
		t.Fatalf("%+v", m)
	}
	for k, v := range want {
		if m.Params[k] != v {
			t.Errorf("%s = %v, want %v", k, m.Params[k], v)
		}
	}
	c = result.Check{}
	offPathNote(&c, p, 40, map[string]tcpRef{})
	if len(c.NotProven) != 0 {
		t.Errorf("a note without tcp-load: %+v", c.NotProven)
	}
}
