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

// Rows follow the "Status rules" tables of docs/methods/tcp-load.md and
// quic-load.md, in order; the two differ only in quic-load's inference on rule 1.
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
		{"2 this host fell behind", with(func(p *phaseResult) { p.lagOver = 51 }), valid, result.Invalid, "load.warning.host_lag"},
		{"2 this host's CPU was full", with(func(p *phaseResult) { p.cpuBusy = 0.9 }), valid, result.Invalid, "load.warning.host_cpu"},
		{"2 the grant held the load", with(func(p *phaseResult) { p.capped, p.gpConf = true, []int{result.Medium} }), valid, result.Invalid, "load.warning.grant_connections"},
		{"2 the node fell behind", clean(), validity{slipSent: 1000, node: &protocol.LoadCounts{LagTicks: 1000, LagOver: 51}, cpuMax: 0.875}, result.Invalid, "load.warning.node_lag"},
		{"2 the probes were sent late", clean(), validity{slipped: 11, slipSent: 1000, cpuMax: 0.875}, result.Invalid, "baseline.warning.send_slip"},
		{"3 a phase broke", with(func(p *phaseResult) { p.stopped, p.err = stopError, broke }), valid, result.Warn, "load.inference.aborted"},
		{"3 a phase was interrupted", with(func(p *phaseResult) { p.stopped = stopInterrupted }), valid, result.Warn, "load.inference.interrupted"},
		{"3 goodput not settled", with(func(p *phaseResult) { p.stopped, p.gpConf = stopBudget, []int{result.Low} }), valid, result.Warn, "load.inference.not_saturated"},
		{"3 responsiveness not settled", with(func(p *phaseResult) { p.stopped, p.rpmConf = stopTime, result.Medium }), valid, result.Warn, "load.inference.rpm_unsettled"},
		{"3 poor responsiveness", with(func(p *phaseResult) { p.rpm.rpm = 299 }), valid, result.Warn, "load.inference.rpm_poor"},
		{"3 load connections that did not open", with(func(p *phaseResult) { p.connFailed = 2 }), valid, result.Warn, "load.inference.connect_failures"},
		{"4 clean", clean(), valid, result.Pass, ""},
		{"a capped phase that settled is valid", with(func(p *phaseResult) { p.capped = true }), valid, result.Pass, ""},
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
