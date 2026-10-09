package load

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/i18n"
	"github.com/Lynthar/ConnVerifier/internal/probe/echo"
	"github.com/Lynthar/ConnVerifier/internal/protocol"
	"github.com/Lynthar/ConnVerifier/internal/report"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

var update = flag.Bool("update", false, "rewrite golden files")

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func at(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }

func durations(base time.Duration, step time.Duration, n int) []time.Duration {
	var d []time.Duration
	for i := range n {
		d = append(d, base+time.Duration(i%20)*step)
	}
	return d
}

// syntheticPhases are made-up results: download and upload settle, both
// directions run into the time limit with poor, unsettled responsiveness.
func syntheticPhases() []phaseResult {
	down, up := settled(phases[0]), settled(phases[1])
	both := settled(phases[2])
	down.start, down.settledAt, down.end = at(0), at(8), at(12)
	down.dirs = []*direction{{down: true, finalBPS: 93.456e6}}
	down.conns, down.bytes = 12, 131_234_567
	down.rpm = responsiveness{rpm: 812.4, foreign: 1100.2, loaded: 524.6, samples: 160, ok: true}
	down.self, down.foreign = durations(110*time.Millisecond, 3*time.Millisecond, 80), durations(65*time.Millisecond, 2*time.Millisecond, 80)
	up.start, up.settledAt, up.end = at(14), at(21), at(26)
	up.dirs = []*direction{{finalBPS: 19.876e6}}
	up.conns, up.bytes = 9, 29_876_543
	up.rpm = responsiveness{rpm: 431.7, foreign: 610.3, loaded: 253.1, samples: 200, ok: true}
	up.self, up.foreign = durations(230*time.Millisecond, 5*time.Millisecond, 100), durations(95*time.Millisecond, 3*time.Millisecond, 100)
	both.start, both.settledAt, both.end = at(28), at(37), at(48)
	both.stopped, both.rpmConf = stopTime, result.Medium
	both.dirs = []*direction{{down: true, finalBPS: 71.002e6}, {finalBPS: 18.204e6}}
	both.conns, both.bytes = 26, 245_678_901
	both.rpm = responsiveness{rpm: 247.9, foreign: 330.5, loaded: 165.3, samples: 220, ok: true}
	both.self, both.foreign = durations(330*time.Millisecond, 8*time.Millisecond, 110), durations(160*time.Millisecond, 4*time.Millisecond, 110)
	up.waited = 2500 * time.Millisecond
	both.waited, both.drained = 10*time.Second, false
	both.connFailed, both.foreignFailed = 2, 3
	for i, p := range []*phaseResult{&down, &up, &both} {
		p.lagTicks, p.lagOver, p.cpuBusy = 1200, uint64(i), 0.31+0.1*float64(i)
	}
	return []phaseResult{down, up, both}
}

// fillStream sends one probe every 20 ms over 50 s: 31 ms round trips at rest,
// 150–190 ms inside the phases, every 100th lost.
func fillStream(s *echo.Stream, p []phaseResult) {
	for i := range 2500 {
		sent := t0.Add(time.Duration(i) * 20 * time.Millisecond)
		pr := echo.Probe{Planned: sent, Sent: sent}
		rtt := 31 * time.Millisecond
		for _, ph := range p {
			if !sent.Before(ph.start) && !sent.After(ph.end) {
				rtt = 150*time.Millisecond + time.Duration(i%5)*10*time.Millisecond
			}
		}
		if i%100 != 99 {
			pr.Replies = []echo.Reply{{At: sent.Add(rtt), ReflSeq: uint32(i)}}
			s.Arrivals = append(s.Arrivals, i)
		}
		s.Probes = append(s.Probes, pr)
	}
}

func goldenCheck() result.Check {
	cfg := Config{interval: time.Second, phaseTime: 20 * time.Second, phaseMB: 500, rate: 50, slipLimit: echo.DefaultSlipLimit, cpuMax: 0.875}
	ph := syntheticPhases()
	ur, tr := &echo.UDPRun{End: at(60)}, &echo.TCPRun{End: at(60)}
	fillStream(&ur.Stream, ph)
	fillStream(&tr.Stream, ph)
	c := result.Check{
		ID: "tcp-load", MethodVersion: 1, ElapsedMs: 52_400,
		Path:    result.Path{Node: "192.0.2.10:7443", Family: "ipv4", Protocol: "tcp"},
		Params:  cfg.params(),
		Metrics: []result.Metric{},
		Node: &result.NodeReport{
			Label: "example", Version: "v0.0.0-golden", ObservedAddr: "198.51.100.7:51234",
			Granted:   &result.Grant{Connections: 1, DialRate: 1, DurationS: 82, IdleTimeoutS: 60, StampRate: 100, LoadBytes: 1_510_000_000, LoadConnections: 40},
			LoadStart: &result.NodeLoad{Sessions: 1, MaxConnections: 20000, UptimeS: 3700},
			LoadEnd:   &result.NodeLoad{MaxConnections: 20000, UptimeS: 3752},
			Traffic:   &result.Traffic{SentBytes: 235_123_456, ReceivedBytes: 171_234_567, LagTicks: 5200, LagOver: 3},
		},
	}
	base := idle{udpP50: 31.2, udpP95: 35.4, tcpP50: 31.5, tcpP95: 36.1}
	finish(&c, ph, ur, tr, nil, &protocol.LoadCounts{LagTicks: 5200, LagOver: 3}, base, cfg)
	return c
}

func TestResultGolden(t *testing.T) {
	run := result.Run{
		Schema: result.Schema, Tool: result.Tool{Name: "connverifier", Version: "v0.0.0-golden", Go: "go1.27.0", OS: "linux", Arch: "amd64"},
		RunID: "golden", Started: t0, Ended: at(52.4), ElapsedMs: 52_400,
		Checks: []result.Check{goldenCheck()},
	}
	outputs := map[string]func(*bytes.Buffer) error{
		"run.json": func(b *bytes.Buffer) error { return report.JSON(b, run) },
	}
	for _, lang := range i18n.Supported {
		cat, err := i18n.Load(lang)
		if err != nil {
			t.Fatal(err)
		}
		outputs["run."+lang+".txt"] = func(b *bytes.Buffer) error { return report.Text(b, run, cat) }
	}
	for name, render := range outputs {
		t.Run(name, func(t *testing.T) {
			var got bytes.Buffer
			if err := render(&got); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", name)
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Fatalf("%s differs from golden; run with -update after checking the change\n--- got ---\n%s", name, got.String())
			}
		})
	}
}

// Every message any rule can produce resolves in every catalog; a missing key
// renders as itself and would show up here.
func TestTextHasNoRawKeys(t *testing.T) {
	broke := errors.New("reset")
	var checks []result.Check
	add := func(f func(*result.Check)) {
		c := result.Check{ID: "tcp-load", Path: result.Path{Node: "192.0.2.10:7443"}, Params: Config{}.params()}
		f(&c)
		checks = append(checks, c)
	}
	add(func(c *result.Check) { *c = goldenCheck() })
	for _, f := range []func(*phaseResult){
		func(p *phaseResult) { p.lagOver, p.cpuBusy, p.capped, p.gpConf = 50, 0.95, true, []int{result.Low} },
		func(p *phaseResult) { p.stopped, p.err = stopError, broke },
		func(p *phaseResult) { p.stopped = stopInterrupted },
		func(p *phaseResult) { p.stopped, p.gpConf, p.rpmConf = stopNodeBudget, []int{result.Low}, result.Low },
		func(p *phaseResult) { p.rpm.rpm = 100 },
	} {
		ph := with(f)
		add(func(c *result.Check) {
			evaluate(c, ph, validity{slipped: 20, slipSent: 1000, node: &protocol.LoadCounts{LagTicks: 100, LagOver: 50}, cpuMax: 0.875})
		})
		add(func(c *result.Check) { evaluate(c, ph, validity{slipSent: 1000, cpuMax: 0.875}) })
	}
	add(func(c *result.Check) {
		evaluate(c, []phaseResult{{spec: phases[0]}, {spec: phases[1]}, {spec: phases[2]}}, validity{})
	})
	add(func(c *result.Check) {
		p := clean()
		for i := range p {
			p[i].stopped, p[i].err = stopError, broke
		}
		evaluate(c, p, validity{})
	})
	for _, key := range []string{"load.unsupported.node_without_load", "load.not_proven.no_udp_probe", "load.warning.duration_not_granted"} {
		add(func(c *result.Check) {
			c.Warnings = []result.Message{{Key: key, Params: map[string]any{"granted_ms": 5000}}}
		})
	}
	for _, reason := range []string{protocol.ErrReasonBusy, protocol.ErrReasonQuota} {
		add(func(c *result.Check) {
			c.Error = &result.Message{Key: "load.error.refused_" + reason, Params: map[string]any{"retry_after_ms": 60000}}
		})
	}
	add(func(c *result.Check) {
		c.NotProven = []result.Message{{Key: "load.not_proven.no_tcp_probe", Params: map[string]any{"error": "x"}}}
		c.Warnings = []result.Message{Config{phaseMB: 500}.Notice()}
	})
	run := result.Run{Checks: checks}
	rawKey := regexp.MustCompile(`\b(load|baseline|scope|stop|level|metric|param|label|section|status|check|line|value|node)\.[A-Za-z_]`)
	for _, lang := range i18n.Supported {
		cat, err := i18n.Load(lang)
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		report.Text(&b, run, cat)
		if m := rawKey.FindString(b.String()); m != "" {
			t.Errorf("%s text contains raw key %q", lang, m)
		}
	}
}
