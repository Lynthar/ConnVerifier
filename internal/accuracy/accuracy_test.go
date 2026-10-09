//go:build accuracy && linux

// Package accuracy checks measured values against conditions injected with
// netem and tbf between two network namespaces. It needs root, iproute2, ethtool
// and ping, runs for about two hours, and is not part of the commit gate.
package accuracy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/result"
)

const (
	nodeNS, clientNS = "cvacc-node", "cvacc-client"
	nodeIf, clientIf = "cvacc-n", "cvacc-c"
	nodeIP           = "192.0.2.1"
	clientIP         = "192.0.2.2"
)

var bin string

func TestMain(m *testing.M) {
	if os.Geteuid() != 0 {
		fmt.Println("accuracy tests need root: sudo go test -tags accuracy ./internal/accuracy")
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "cvacc")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "connverifier")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/connverifier").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("build: %v\n%s", err, out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func sh(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// lab is two namespaces joined by a veth pair, with a node serving in one.
type lab struct {
	t      *testing.T
	invite string
}

// newLab starts a node whose invite is created with inviteArgs.
func newLab(t *testing.T, inviteArgs ...string) *lab {
	t.Helper()
	exec.Command("ip", "netns", "del", nodeNS).Run()
	exec.Command("ip", "netns", "del", clientNS).Run()
	sh(t, "ip", "netns", "add", nodeNS)
	sh(t, "ip", "netns", "add", clientNS)
	t.Cleanup(func() {
		exec.Command("ip", "netns", "del", nodeNS).Run()
		exec.Command("ip", "netns", "del", clientNS).Run()
	})
	sh(t, "ip", "link", "add", nodeIf, "netns", nodeNS, "type", "veth", "peer", "name", clientIf, "netns", clientNS)
	for _, x := range []struct{ ns, ifc, ip string }{{nodeNS, nodeIf, nodeIP}, {clientNS, clientIf, clientIP}} {
		sh(t, "ip", "-n", x.ns, "addr", "add", x.ip+"/24", "dev", x.ifc)
		sh(t, "ip", "-n", x.ns, "link", "set", x.ifc, "up")
		sh(t, "ip", "-n", x.ns, "link", "set", "lo", "up")
		// Segmentation offload hands the qdisc 64 KB super-packets; shaping and
		// the payload ceiling below assume packets of one MTU.
		sh(t, "ip", "netns", "exec", x.ns, "ethtool", "-K", x.ifc, "tso", "off", "gso", "off")
	}
	state := filepath.Join(t.TempDir(), "node")
	cmd := exec.Command(bin, append([]string{"invite", "create", "-state-dir", state, "-label", "acc", "-addr", nodeIP + ":7443"}, inviteArgs...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("invite create: %v", err)
	}
	l := &lab{t: t, invite: strings.TrimSpace(string(out))}
	serve := exec.Command("ip", "netns", "exec", nodeNS, bin, "serve", "-state-dir", state)
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serve.Process.Kill(); serve.Wait() })
	time.Sleep(500 * time.Millisecond)
	return l
}

// netem replaces the qdisc on the forward (client → node) or return (node →
// client) direction; an empty spec removes it.
func (l *lab) netem(forward bool, spec string) {
	ns, ifc := nodeNS, nodeIf
	if forward {
		ns, ifc = clientNS, clientIf
	}
	exec.Command("tc", "-n", ns, "qdisc", "del", "dev", ifc, "root").Run()
	if spec != "" {
		sh(l.t, append([]string{"tc", "-n", ns, "qdisc", "add", "dev", ifc, "root", "netem"}, strings.Fields(spec)...)...)
	}
}

// shape replaces the qdisc on one direction with netem (delay, the netem
// arguments after "delay"), then a token bucket of rate with a queue of queue (tbf
// latency), with fq_codel inside it when fq.
func (l *lab) shape(forward bool, delay, rate, queue string, fq bool) {
	ns, ifc := nodeNS, nodeIf
	if forward {
		ns, ifc = clientNS, clientIf
	}
	exec.Command("tc", "-n", ns, "qdisc", "del", "dev", ifc, "root").Run()
	sh(l.t, append([]string{"tc", "-n", ns, "qdisc", "add", "dev", ifc, "root", "handle", "1:", "netem", "limit", "100000", "delay"}, strings.Fields(delay)...)...)
	// A burst of 10 ms at rate keeps the queue at what latency declares; a fixed
	// 64 kB added 26 ms at 20 Mbit/s.
	bits, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(rate, "bit"), "m"), "g"), 64)
	switch {
	case err != nil:
		l.t.Fatalf("rate %q", rate)
	case strings.HasSuffix(rate, "mbit"):
		bits *= 1e6
	case strings.HasSuffix(rate, "gbit"):
		bits *= 1e9
	}
	burst := fmt.Sprint(max(int(bits/8/100), 3000))
	sh(l.t, "tc", "-n", ns, "qdisc", "add", "dev", ifc, "parent", "1:1", "handle", "10:", "tbf", "rate", rate, "burst", burst, "latency", queue)
	if fq {
		sh(l.t, "tc", "-n", ns, "qdisc", "add", "dev", ifc, "parent", "10:1", "handle", "20:", "fq_codel")
	}
}

// baselineOnly keeps the load stage of a check to a second per phase, for the
// scenarios that judge the idle checks only.
var baselineOnly = []string{"-load-time", "1s", "-load-mb", "1"}

// check runs connverifier check from the client namespace with args and returns
// the UDP, TCP and load results; prefix wraps the command (such as a CPU-limited
// scope).
func (l *lab) check(args []string, prefix ...string) (udp, tcp, load result.Check) {
	l.t.Helper()
	args = append(append(prefix, "ip", "netns", "exec", clientNS, bin, "check", "-format", "json"), args...)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "CONNVERIFIER_NODE="+l.invite)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Run()
	var run result.Run
	if err := json.Unmarshal(stdout.Bytes(), &run); err != nil || len(run.Checks) != 3 {
		l.t.Fatalf("check output: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	return run.Checks[0], run.Checks[1], run.Checks[2]
}

func value(c result.Check, id string) (v float64, m result.Metric) {
	for _, m := range c.Metrics {
		if m.ID == id && m.Value != nil {
			return *m.Value, m
		}
	}
	return -1, result.Metric{}
}

func covers(m result.Metric, pct float64) bool {
	return m.Low != nil && m.High != nil && *m.Low <= pct && pct <= *m.High
}

// pingRTT starts n ICMP echoes 20 ms apart from the client namespace; the returned
// function waits and gives the minimum, p50 and p99 in ms — the reference the
// environment allows, sampled over the same seconds as the check beside it.
func (l *lab) pingRTT(n int) func() (lo, p50, p99 float64) {
	l.t.Helper()
	cmd := exec.Command("ip", "netns", "exec", clientNS, "ping", "-c", fmt.Sprint(n), "-i", "0.02", nodeIP)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		l.t.Fatalf("ping: %v", err)
	}
	return func() (lo, p50, p99 float64) {
		cmd.Wait()
		var rtt []float64
		for _, line := range strings.Split(out.String(), "\n") {
			if _, after, ok := strings.Cut(line, "time="); ok {
				var v float64
				fmt.Sscanf(after, "%f", &v)
				rtt = append(rtt, v)
			}
		}
		slices.Sort(rtt)
		if len(rtt) < 100 {
			l.t.Fatalf("ping: %d replies", len(rtt))
		}
		at := func(q float64) float64 { return rtt[int(math.Ceil(q*float64(len(rtt))))-1] }
		return rtt[0], at(0.5), at(0.99)
	}
}

// S1: 50 ms injected, no loss. Netem's own jitter depends on the host, so each of
// 10 runs is judged against ping running beside it: minimum within 0.5 ms above
// ping's, p50 within 0.5 ms of it, p99 at most 1.5 ms above it.
func TestS1Delay(t *testing.T) {
	l := newLab(t)
	l.netem(true, "delay 25ms")
	l.netem(false, "delay 25ms")
	for run := range 10 {
		ref := l.pingRTT(1000)
		udp, tcp, _ := l.check(append([]string{"-duration", "20s"}, baselineOnly...))
		refMin, refP50, refP99 := ref()
		for _, c := range []result.Check{udp, tcp} {
			lo, _ := value(c, "rtt.min")
			p50, _ := value(c, "rtt.p50")
			p99, _ := value(c, "rtt.p99")
			t.Logf("run %d %s: %s min %.3f (ping %.3f) p50 %.3f (%.3f) p99 %.3f (%.3f)", run, c.ID, c.Status, lo, refMin, p50, refP50, p99, refP99)
			if c.Status != result.Pass || lo < 50 || lo > refMin+0.5 || math.Abs(p50-refP50) > 0.5 || p99 > refP99+1.5 {
				t.Errorf("run %d %s outside the contract", run, c.ID)
			}
		}
	}
}

// lossCoverage runs 20 times with loss injected in one direction and requires
// the injected 1% inside the reported 95% interval in at least 17 runs, and no
// loss in the other direction in any run.
func lossCoverage(t *testing.T, forward bool, spec, metric, other string) {
	l := newLab(t)
	l.netem(forward, "delay 25ms "+spec)
	l.netem(!forward, "delay 25ms")
	hits := 0
	for run := range 20 {
		udp, tcp, _ := l.check(append([]string{"-duration", "30s"}, baselineOnly...))
		v, m := value(udp, metric)
		o, _ := value(udp, other)
		p99, _ := value(tcp, "rtt.p99")
		t.Logf("run %d: %s %.2f%% [%.2f, %.2f] %s %.0f; tcp p99 %.1f ms %s", run, metric, v, *m.Low, *m.High, other, o, p99, tcp.Status)
		if covers(m, 1) {
			hits++
		}
		if o != 0 {
			t.Errorf("run %d: %s = %v, want 0", run, other, o)
		}
	}
	if hits < 17 {
		t.Errorf("1%% inside the interval in %d of 20 runs, want ≥ 17", hits)
	}
}

func TestS2ForwardLoss(t *testing.T) { lossCoverage(t, true, "loss 1%", "loss.forward", "lost.return") }
func TestS3ReturnLoss(t *testing.T)  { lossCoverage(t, false, "loss 1%", "loss.return", "lost.forward") }

// S4: 1% duplication on the way there — all attributed forward, ratio covering 1%.
func TestS4ForwardDuplication(t *testing.T) {
	l := newLab(t)
	l.netem(true, "delay 25ms duplicate 1%")
	l.netem(false, "delay 25ms")
	hits := 0
	for run := range 20 {
		udp, _, _ := l.check(append([]string{"-duration", "20s"}, baselineOnly...))
		v, m := value(udp, "duplication")
		ret, _ := value(udp, "duplicates.return")
		t.Logf("run %d: duplication %.2f%% [%.2f, %.2f] return %.0f", run, v, *m.Low, *m.High, ret)
		if covers(m, 1) {
			hits++
		}
		if ret != 0 {
			t.Errorf("run %d: %v duplicates attributed to the way back", run, ret)
		}
	}
	if hits < 17 {
		t.Errorf("1%% inside the interval in %d of 20 runs, want ≥ 17", hits)
	}
}

// S5: delay jitter large enough to reorder on the way there.
func TestS5ForwardReordering(t *testing.T) {
	l := newLab(t)
	l.netem(true, "delay 25ms 10ms")
	l.netem(false, "delay 25ms")
	udp, _, _ := l.check(append([]string{"-duration", "20s"}, baselineOnly...))
	fwd, _ := value(udp, "reordered.forward")
	pdv, _ := value(udp, "pdv.p99")
	t.Logf("forward reordered %.0f, pdv.p99 %.2f ms, %s", fwd, pdv, udp.Status)
	if fwd <= 0 || pdv <= 1 {
		t.Errorf("forward reordering %v, PDV p99 %v; want both above zero and 1 ms", fwd, pdv)
	}
}

// S6: a client starved of CPU cannot keep its schedule: INVALID, for that reason.
func TestS6ThrottledClient(t *testing.T) {
	l := newLab(t)
	udp, tcp, _ := l.check(append([]string{"-duration", "20s"}, baselineOnly...), "systemd-run", "--quiet", "--scope", "-p", "CPUQuota=1%")
	for _, c := range []result.Check{udp, tcp} {
		var keys []string
		for _, w := range c.Warnings {
			keys = append(keys, w.Key)
		}
		t.Logf("%s: %s %v", c.ID, c.Status, keys)
		if c.Status != result.Invalid || !strings.Contains(strings.Join(keys, " "), "baseline.warning.send_slip") {
			t.Errorf("%s: %s %v, want INVALID for send slip", c.ID, c.Status, keys)
		}
	}
}

// S7: nothing injected — PASS, and no loss, duplication or reordering.
func TestS7Clean(t *testing.T) {
	l := newLab(t)
	udp, tcp, _ := l.check(append([]string{"-duration", "20s"}, baselineOnly...))
	for _, id := range []string{"lost", "duplicates.forward", "duplicates.return", "reordered"} {
		if v, _ := value(udp, id); v != 0 {
			t.Errorf("%s = %v on a clean path", id, v)
		}
	}
	if udp.Status != result.Pass || tcp.Status != result.Pass {
		t.Errorf("udp %s, tcp %s", udp.Status, tcp.Status)
	}
}

// ceiling is the goodput a tbf of rate bit/s allows, in Mbit/s: tbf counts
// Ethernet frames of 1514 bytes carrying 1448 of TCP payload (timestamps on), and
// a full TLS record carries 16384 bytes of payload in 16406.
func ceiling(rate float64) float64 { return rate / 1e6 * 1448 / 1514 * 16384 / 16406 }

func warnings(c result.Check) string {
	var k []string
	for _, w := range c.Warnings {
		k = append(k, w.Key)
	}
	return strings.Join(k, " ")
}

// summary is a load check's status, messages and lag, for the log.
func summary(c result.Check) string {
	var k []string
	for _, m := range append(append([]result.Message{}, c.Warnings...), c.Inferences...) {
		k = append(k, fmt.Sprintf("%s%v", m.Key, m.Params))
	}
	ticks, _ := value(c, "lag.ticks")
	late, _ := value(c, "lag.over_10ms")
	cpu, _ := value(c, "cpu.busy_max")
	return fmt.Sprintf("%s lag %.0f/%.0f cpu %.1f%% %s", c.Status, late, ticks, cpu, strings.Join(k, " "))
}

// hostBound reports whether the load check found this host or the node too slow.
func hostBound(c result.Check) bool {
	w := warnings(c)
	return strings.Contains(w, "load.warning.host_") || strings.Contains(w, "load.warning.node_lag")
}

// goodputRuns shapes both directions to down and up bit/s over a 20 ms round
// trip and requires, in each of runs runs, settled download and upload goodput
// within [0.93, 1.005] of their ceilings, and no host or node limit reported. The
// floor leaves the probes their share: up to 5% of goodput, and the independent
// probes about 0.1 Mbit/s, 1% of a 10 Mbit/s link.
func goodputRuns(t *testing.T, down, up float64, runs int) {
	l := newLab(t)
	l.shape(false, "10ms", fmt.Sprint(down), "40ms", false)
	l.shape(true, "10ms", fmt.Sprint(up), "40ms", false)
	for run := range runs {
		_, _, ld := l.check([]string{"-duration", "10s"})
		for _, x := range []struct {
			phase string
			ceil  float64
		}{{"download", ceiling(down)}, {"upload", ceiling(up)}} {
			v, m := value(ld, x.phase+".goodput")
			conf, _ := value(ld, x.phase+".goodput.confidence")
			t.Logf("run %d %s: %.2f Mbit/s (ceiling %.2f, %.1f%%) confidence %v at least %v", run, x.phase, v, x.ceil, v/x.ceil*100, conf, m.AtLeast)
			if v < 0.93*x.ceil || v > 1.005*x.ceil || conf != result.High || m.AtLeast {
				t.Errorf("run %d %s outside the contract", run, x.phase)
			}
		}
		bd, _ := value(ld, "bidirectional.goodput.download")
		bu, _ := value(ld, "bidirectional.goodput.upload")
		t.Logf("run %d both: %.2f / %.2f Mbit/s; %s", run, bd, bu, summary(ld))
		if bd > 1.005*ceiling(down) || bu > 1.005*ceiling(up) {
			t.Errorf("run %d: both directions above their ceilings", run)
		}
		if hostBound(ld) {
			t.Errorf("run %d: host or node reported as the limit at %v bit/s: %s", run, down, warnings(ld))
		}
	}
}

// L1, L2 and L3. L2 is also L7: unthrottled CPU at 100 Mbit/s must never be
// reported as this host's limit.
func TestL1Goodput20(t *testing.T)        { goodputRuns(t, 20e6, 20e6, 10) }
func TestL2Goodput100(t *testing.T)       { goodputRuns(t, 100e6, 100e6, 10) }
func TestL3Asymmetric100_10(t *testing.T) { goodputRuns(t, 100e6, 10e6, 5) }

// queueRuns shapes both directions to 20 Mbit/s over a 20 ms round trip with a
// 200 ms tbf queue, with fq_codel inside it when fq, and returns the load checks.
func queueRuns(t *testing.T, fq bool, runs int) (idleUDP []float64, loads []result.Check) {
	l := newLab(t)
	l.shape(false, "10ms", "20mbit", "200ms", fq)
	l.shape(true, "10ms", "20mbit", "200ms", fq)
	for range runs {
		udp, _, ld := l.check([]string{"-duration", "10s"})
		p50, _ := value(udp, "rtt.p50")
		idleUDP = append(idleUDP, p50)
		loads = append(loads, ld)
	}
	return idleUDP, loads
}

// L4: a 200 ms FIFO fills; the independent probes see it and RPM is poor.
func TestL4BloatedQueue(t *testing.T) {
	_, loads := queueRuns(t, false, 3)
	for run, ld := range loads {
		for _, phase := range []string{"download", "upload"} {
			udp, _ := value(ld, phase+".udp.rtt.p50")
			tcp, _ := value(ld, phase+".tcp.rtt.p50")
			rpm, _ := value(ld, phase+".rpm")
			t.Logf("run %d %s: udp p50 %.1f tcp p50 %.1f ms, %.0f RPM; %s", run, phase, udp, tcp, rpm, summary(ld))
			if udp < 170 || udp > 240 || tcp < 170 || tcp > 240 || rpm <= 0 || rpm >= 300 {
				t.Errorf("run %d %s outside the contract", run, phase)
			}
		}
	}
}

// L5: the same queue under fq_codel: the independent probes, sparse flows, stay
// near idle while requests on the load connections wait behind their own data.
func TestL5FlowQueuing(t *testing.T) {
	idle, loads := queueRuns(t, true, 3)
	for run, ld := range loads {
		for _, phase := range []string{"download", "upload"} {
			udp, _ := value(ld, phase+".udp.rtt.p50")
			self, _ := value(ld, phase+".self.p50")
			foreign, _ := value(ld, phase+".foreign.p50")
			t.Logf("run %d %s: udp p50 %.1f (idle %.1f) self %.1f foreign %.1f ms", run, phase, udp, idle[run], self, foreign)
			if udp > idle[run]+15 || self <= foreign {
				t.Errorf("run %d %s outside the contract", run, phase)
			}
		}
	}
}

// L6: a client held to a tenth of a core behind a 1 Gbit/s link is reported as
// the limit, not the network.
func TestL6ThrottledClient(t *testing.T) {
	l := newLab(t)
	l.shape(false, "1ms", "1gbit", "20ms", false)
	l.shape(true, "1ms", "1gbit", "20ms", false)
	for run := range 10 {
		_, _, ld := l.check([]string{"-duration", "10s"}, "systemd-run", "--quiet", "--scope", "-p", "CPUQuota=10%")
		t.Logf("run %d: %s", run, summary(ld))
		if ld.Status != result.Invalid || !strings.Contains(warnings(ld), "load.warning.host_") {
			t.Errorf("run %d: %s, want INVALID for this host", run, ld.Status)
		}
	}
}

// L8: a node grant of 50 MB ends every phase at its share, reported as a lower
// bound, and the node never moves more than the grant.
func TestL8NodeBudget(t *testing.T) {
	l := newLab(t, "-max-load-mb", "50")
	l.shape(false, "10ms", "100mbit", "40ms", false)
	l.shape(true, "10ms", "100mbit", "40ms", false)
	_, _, ld := l.check([]string{"-duration", "10s"})
	for _, id := range []string{"download.goodput", "upload.goodput", "bidirectional.goodput.download"} {
		v, m := value(ld, id)
		t.Logf("%s %.2f Mbit/s at least %v", id, v, m.AtLeast)
		if !m.AtLeast {
			t.Errorf("%s is not reported as a lower bound", id)
		}
	}
	tr := ld.Node.Traffic
	if tr == nil || tr.SentBytes+tr.ReceivedBytes > 50e6 {
		t.Fatalf("node traffic %+v, want at most 50 MB", tr)
	}
}

// L9: a bloated, lossy uplink like a real one measured — 10 Mbit/s with a 2 s
// queue and 1% loss under a 100 Mbit/s downlink. No phase may be aborted for
// connections that open slowly, and the upload goodput is reported without
// counting what is still in the queue.
func TestL9BloatedUplink(t *testing.T) {
	l := newLab(t)
	l.shape(false, "10ms", "100mbit", "40ms", false)
	l.shape(true, "10ms loss 1%", "10mbit", "2s", false)
	for run := range 3 {
		_, _, ld := l.check([]string{"-duration", "10s"})
		up, m := value(ld, "upload.goodput")
		wait, _ := value(ld, "bidirectional.wait")
		failed, _ := value(ld, "upload.connect_failures")
		t.Logf("run %d: upload %.2f Mbit/s (ceiling %.2f) at least %v, %v connections did not open, waited %.0f ms; %s", run, up, ceiling(10e6), m.AtLeast, failed, wait, summary(ld))
		var inferred []string
		for _, i := range ld.Inferences {
			inferred = append(inferred, i.Key)
		}
		if ld.Status == result.Error || slices.Contains(inferred, "load.inference.aborted") || up <= 0 || up > 1.005*ceiling(10e6) {
			t.Errorf("run %d outside the contract", run)
		}
	}
}
