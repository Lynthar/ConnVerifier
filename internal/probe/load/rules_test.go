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

// quicUploads are clean quic-load phases whose upload directions moved mbps with
// sender loss lossPct, against a tcp-load reference of 100 Mbit/s at 0.5% loss in
// both phases with an upload.
func quicUploads(mbps, lossPct float64) ([]phaseResult, validity) {
	p := clean()
	for i := range p {
		if p[i].spec.down {
			p[i].dirs = append(p[i].dirs, &direction{down: true, finalBPS: 400e6})
		}
		if p[i].spec.up {
			p[i].dirs = append(p[i].dirs, &direction{finalBPS: mbps * 1e6, sent: 100_000, resent: uint64(lossPct * 1000), sentKnown: true})
		}
	}
	ref := map[string]tcpUpload{"upload": {bps: 100e6, loss: 0.5, lossKnown: true}, "bidirectional": {bps: 100e6, loss: 0.5, lossKnown: true}}
	return p, validity{slipSent: 1000, node: &protocol.LoadCounts{LagTicks: 1000}, cpuMax: 0.875, quic: true, tcp: ref}
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
		{"no TCP reference is noted", func() []phaseResult { p, _ := quicUploads(60, 5); return p }(), validity{slipSent: 1000, cpuMax: 0.875, quic: true, tcp: map[string]tcpUpload{}}, result.Pass, "quic_load.not_proven.quic_held_no_tcp"},
		{"lossless TCP is no reference", func() []phaseResult { p, _ := quicUploads(60, 5); return p }(), validity{slipSent: 1000, cpuMax: 0.875, quic: true, tcp: map[string]tcpUpload{"upload": {bps: 100e6, lossKnown: true}, "bidirectional": {bps: 100e6, lossKnown: true}}}, result.Pass, "quic_load.not_proven.quic_held_tcp_lossless"},
		{"no sender counters is noted", func() []phaseResult {
			p, _ := quicUploads(60, 5)
			for i := range p {
				for _, d := range p[i].dirs {
					d.sentKnown = false
				}
			}
			return p
		}(), func() validity { _, v := quicUploads(60, 5); return v }(), result.Pass, "quic_load.not_proven.quic_held_no_counters"},
		{"quic-load notes the download is not judged", func() []phaseResult { p, _ := quicUploads(100, 0.5); return p }(), func() validity { _, v := quicUploads(100, 0.5); return v }(), result.Pass, "quic_load.not_proven.quic_held_download"},
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
