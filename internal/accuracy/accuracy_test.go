//go:build accuracy && linux

// Package accuracy checks measured values against conditions injected with
// netem and tbf between two network namespaces. It needs root, iproute2, ethtool,
// nftables, setpriv and ping, runs for about four hours, and is not part of the
// commit gate.
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
	os.Chmod(dir, 0o755) // Q-B2 runs the client as nobody
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
// clientAs, when set, wraps the client inside its namespace (such as setpriv),
// and stderr holds what the last check wrote there.
type lab struct {
	t        *testing.T
	invite   string
	clientAs []string
	stderr   string
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
	// Ten runs of both loads at the default session grant move up to 200 GB, past the
	// default daily quota.
	args := append([]string{"invite", "create", "-state-dir", state, "-label", "acc", "-addr", nodeIP + ":7443", "-max-load-mb-per-day", "1000000"}, inviteArgs...)
	cmd := exec.Command(bin, args...)
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

// police makes each direction a 10 ms netem delay and, at the receiving end, a
// policer that drops above rate (Mbit/s) with 5 ms of burst: a bottleneck without a
// queue. GRO is off there, so the policer meters the packets as sent.
func (l *lab) police(mbit int) {
	for _, x := range []struct {
		forward        bool
		recvNS, recvIf string
	}{{true, nodeNS, nodeIf}, {false, clientNS, clientIf}} {
		l.netem(x.forward, "delay 10ms limit 100000")
		sh(l.t, "ip", "netns", "exec", x.recvNS, "ethtool", "-K", x.recvIf, "gro", "off")
		sh(l.t, "tc", "-n", x.recvNS, "qdisc", "add", "dev", x.recvIf, "handle", "ffff:", "ingress")
		sh(l.t, "tc", "-n", x.recvNS, "filter", "add", "dev", x.recvIf, "parent", "ffff:", "matchall", "action", "police",
			"rate", fmt.Sprintf("%dmbit", mbit), "burst", fmt.Sprint(mbit*1e6/8/200), "conform-exceed", "drop")
	}
}

// shallow makes each direction a 10 ms netem delay and a token bucket of rate
// (Mbit/s) with about 2 ms of queue: 1 ms of burst and 1 ms of latency.
func (l *lab) shallow(mbit int) {
	for _, forward := range []bool{true, false} {
		ns, ifc := nodeNS, nodeIf
		if forward {
			ns, ifc = clientNS, clientIf
		}
		exec.Command("tc", "-n", ns, "qdisc", "del", "dev", ifc, "root").Run()
		sh(l.t, "tc", "-n", ns, "qdisc", "add", "dev", ifc, "root", "handle", "1:", "netem", "limit", "100000", "delay", "10ms")
		sh(l.t, "tc", "-n", ns, "qdisc", "add", "dev", ifc, "parent", "1:1", "handle", "10:", "tbf", "rate", fmt.Sprintf("%dmbit", mbit),
			"burst", fmt.Sprint(mbit*1e6/8/1000), "latency", "1ms")
	}
}

// baselineOnly keeps the load stage of a check to a second per phase, for the
// scenarios that judge the idle checks only.
var baselineOnly = []string{"-load-time", "1s", "-load-mb", "1"}

// check runs connverifier check from the client namespace with args and returns
// the UDP and TCP results and the two loads, tcp-load then quic-load; prefix
// wraps the command (such as a CPU-limited scope).
func (l *lab) check(args []string, prefix ...string) (udp, tcp result.Check, loads []result.Check) {
	l.t.Helper()
	args = append(append(append(append(prefix, "ip", "netns", "exec", clientNS), l.clientAs...), bin, "check", "-format", "json", "-yes"), args...)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "CONNVERIFIER_NODE="+l.invite)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Run()
	l.stderr = stderr.String()
	var run result.Run
	if err := json.Unmarshal(stdout.Bytes(), &run); err != nil || len(run.Checks) != 4 {
		l.t.Fatalf("check output: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	return run.Checks[0], run.Checks[1], run.Checks[2:]
}

// loadKinds are the two loads of a check, in its order, with their goodput
// ceilings.
var loadKinds = []struct {
	id      string
	ceiling func(rate float64) float64
}{{"tcp-load", ceiling}, {"quic-load", quicCeiling}}

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

// quicCeiling is the same for QUIC: a 1452-byte UDP payload, quic-go's largest,
// in a 1494-byte frame, less about 7 bytes of short header, a 16-byte AEAD tag
// and 9 of STREAM frame header — an estimate, pinned by the first runs.
func quicCeiling(rate float64) float64 { return rate / 1e6 * 1420 / 1494 }

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
	if c.Error != nil {
		k = append(k, fmt.Sprintf("error %s%v", c.Error.Key, c.Error.Params))
	}
	ticks, _ := value(c, "lag.ticks")
	late, _ := value(c, "lag.over_10ms")
	cpu, _ := value(c, "cpu.busy_max")
	return fmt.Sprintf("%s lag %.0f/%.0f cpu %.1f%% %s", c.Status, late, ticks, cpu, strings.Join(k, " "))
}

// inferred lists a check's inference keys.
func inferred(c result.Check) []string {
	var k []string
	for _, m := range c.Inferences {
		k = append(k, m.Key)
	}
	return k
}

// hostBound reports whether the load check found this host or the node too slow.
func hostBound(c result.Check) bool {
	k := inferred(c)
	return slices.Contains(k, "load.inference.host_limited") || slices.Contains(k, "load.inference.node_limited")
}

// goodputBounded reports whether every goodput of c that has a value is a lower bound.
func goodputBounded(c result.Check) bool {
	for _, m := range c.Metrics {
		if m.Unit == result.UnitMbps && m.Value != nil && !m.AtLeast {
			return false
		}
	}
	return true
}

// goodputRuns shapes both directions to down and up bit/s over a 20 ms round
// trip and requires, in each of runs runs and for each load, settled download
// and upload goodput within [0.93, 1.005] of its ceiling, and no host or node
// limit reported. The floor leaves the probes their share: up to 5% of goodput,
// and the independent probes about 0.1 Mbit/s, 1% of a 10 Mbit/s link. It logs
// QUIC's goodput over TCP's in each run, which Q-C1 records without a bound.
func goodputRuns(t *testing.T, down, up float64, runs int) {
	l := newLab(t)
	l.shape(false, "10ms", fmt.Sprint(down), "40ms", false)
	l.shape(true, "10ms", fmt.Sprint(up), "40ms", false)
	for run := range runs {
		_, _, loads := l.check([]string{"-duration", "10s"})
		for k, ld := range loads {
			kind := loadKinds[k]
			for _, x := range []struct {
				phase string
				ceil  float64
			}{{"download", kind.ceiling(down)}, {"upload", kind.ceiling(up)}} {
				v, m := value(ld, x.phase+".goodput")
				conf, _ := value(ld, x.phase+".goodput.confidence")
				t.Logf("run %d %s %s: %.2f Mbit/s (ceiling %.2f, %.1f%%) confidence %v at least %v", run, kind.id, x.phase, v, x.ceil, v/x.ceil*100, conf, m.AtLeast)
				if v < 0.93*x.ceil || v > 1.005*x.ceil || conf != result.High || m.AtLeast {
					t.Errorf("run %d %s %s outside the contract", run, kind.id, x.phase)
				}
			}
			bd, _ := value(ld, "bidirectional.goodput.download")
			bu, _ := value(ld, "bidirectional.goodput.upload")
			t.Logf("run %d %s both: %.2f / %.2f Mbit/s; %s", run, kind.id, bd, bu, summary(ld))
			if bd > 1.005*kind.ceiling(down) || bu > 1.005*kind.ceiling(up) {
				t.Errorf("run %d %s: both directions above their ceilings", run, kind.id)
			}
			if hostBound(ld) {
				t.Errorf("run %d %s: host or node reported as the limit at %v bit/s: %v", run, kind.id, down, inferred(ld))
			}
		}
		for _, phase := range []string{"download", "upload"} {
			tg, _ := value(loads[0], phase+".goodput")
			qg, _ := value(loads[1], phase+".goodput")
			t.Logf("run %d %s: QUIC/TCP goodput %.3f", run, phase, qg/tg)
		}
	}
}

// L1, L2 and L3, for both loads. L2 is also L7: unthrottled CPU at 100 Mbit/s
// must never be reported as this host's limit; and Q-C1, the QUIC/TCP ratio.
func TestL1Goodput20(t *testing.T)        { goodputRuns(t, 20e6, 20e6, 10) }
func TestL2Goodput100(t *testing.T)       { goodputRuns(t, 100e6, 100e6, 10) }
func TestL3Asymmetric100_10(t *testing.T) { goodputRuns(t, 100e6, 10e6, 5) }

// queueRuns shapes both directions to 20 Mbit/s over a 20 ms round trip with a
// 200 ms tbf queue, with fq_codel inside it when fq, and returns each run's two
// load checks.
func queueRuns(t *testing.T, fq bool, runs int) (idleUDP []float64, loads [][]result.Check) {
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
	for run, lds := range loads {
		for k, ld := range lds {
			for _, phase := range []string{"download", "upload"} {
				udp, _ := value(ld, phase+".udp.rtt.p50")
				tcp, _ := value(ld, phase+".tcp.rtt.p50")
				rpm, _ := value(ld, phase+".rpm")
				t.Logf("run %d %s %s: udp p50 %.1f tcp p50 %.1f ms, %.0f RPM; %s", run, loadKinds[k].id, phase, udp, tcp, rpm, summary(ld))
				if udp < 170 || udp > 240 || tcp < 170 || tcp > 240 || rpm <= 0 || rpm >= 300 {
					t.Errorf("run %d %s %s outside the contract", run, loadKinds[k].id, phase)
				}
			}
		}
	}
}

// L5: the same queue under fq_codel: the independent probes, sparse flows, stay
// near idle; over TCP requests on the load connections wait behind their own data.
// QUIC's self probes wait only for their connection's share: logged, not judged.
func TestL5FlowQueuing(t *testing.T) {
	idle, loads := queueRuns(t, true, 3)
	for run, lds := range loads {
		for k, ld := range lds {
			for _, phase := range []string{"download", "upload"} {
				udp, _ := value(ld, phase+".udp.rtt.p50")
				self, _ := value(ld, phase+".self.p50")
				foreign, _ := value(ld, phase+".foreign.p50")
				t.Logf("run %d %s %s: udp p50 %.1f (idle %.1f) self %.1f foreign %.1f ms", run, loadKinds[k].id, phase, udp, idle[run], self, foreign)
				if udp > idle[run]+15 || loadKinds[k].id == "tcp-load" && self <= foreign {
					t.Errorf("run %d %s %s outside the contract", run, loadKinds[k].id, phase)
				}
			}
		}
	}
}

// L6: a client held to a tenth of a core behind a 1 Gbit/s link is reported as
// the possible limit, not the network: every goodput a lower bound.
func TestL6ThrottledClient(t *testing.T) {
	l := newLab(t)
	l.shape(false, "1ms", "1gbit", "20ms", false)
	l.shape(true, "1ms", "1gbit", "20ms", false)
	for run := range 10 {
		_, _, loads := l.check([]string{"-duration", "10s"}, "systemd-run", "--quiet", "--scope", "-p", "CPUQuota=10%")
		for k, ld := range loads {
			t.Logf("run %d %s: %s", run, loadKinds[k].id, summary(ld))
			if ld.Status != result.Warn || !hostBound(ld) || !goodputBounded(ld) {
				t.Errorf("run %d %s: %s %v, want WARN with this host the possible limit and every goodput a lower bound", run, loadKinds[k].id, ld.Status, inferred(ld))
			}
		}
	}
}

// L8: a node grant of 50 MB ends every phase at its share, reported as a lower
// bound, and the node never moves more than the grant.
func TestL8NodeBudget(t *testing.T) {
	l := newLab(t, "-max-load-mb", "50")
	l.shape(false, "10ms", "100mbit", "40ms", false)
	l.shape(true, "10ms", "100mbit", "40ms", false)
	_, _, loads := l.check([]string{"-duration", "10s"})
	for k, ld := range loads {
		for _, id := range []string{"download.goodput", "upload.goodput", "bidirectional.goodput.download"} {
			v, m := value(ld, id)
			t.Logf("%s %s %.2f Mbit/s at least %v", loadKinds[k].id, id, v, m.AtLeast)
			if !m.AtLeast {
				t.Errorf("%s %s is not reported as a lower bound", loadKinds[k].id, id)
			}
		}
		tr := ld.Node.Traffic
		if tr == nil || tr.SentBytes+tr.ReceivedBytes > 50e6 {
			t.Errorf("%s node traffic %+v, want at most 50 MB", loadKinds[k].id, tr)
		}
	}
}

// L9: a bloated, lossy uplink like a real one measured — 10 Mbit/s with a 2 s
// queue and 1% loss under a 100 Mbit/s downlink. No phase may be aborted for
// connections that open slowly, and the upload goodput is reported without
// counting what is still in the queue. quic-load infers nothing held QUIC apart
// from TCP: with both directions at once, both wait for acknowledgements in that queue.
func TestL9BloatedUplink(t *testing.T) {
	l := newLab(t)
	l.shape(false, "10ms", "100mbit", "40ms", false)
	l.shape(true, "10ms loss 1%", "10mbit", "2s", false)
	for run := range 3 {
		_, _, loads := l.check([]string{"-duration", "10s"})
		logDirections(t, run, loads)
		for k, ld := range loads {
			ceil := loadKinds[k].ceiling(10e6)
			up, m := value(ld, "upload.goodput")
			wait, _ := value(ld, "bidirectional.wait")
			failed, _ := value(ld, "upload.connect_failures")
			t.Logf("run %d %s: upload %.2f Mbit/s (ceiling %.2f) at least %v, %v connections did not open, waited %.0f ms; %s", run, loadKinds[k].id, up, ceil, m.AtLeast, failed, wait, summary(ld))
			var inferred []string
			for _, i := range ld.Inferences {
				inferred = append(inferred, i.Key)
			}
			if ld.Status == result.Error || slices.Contains(inferred, "load.inference.aborted") || up <= 0 || up > 1.005*ceil || len(held(ld)) > 0 {
				t.Errorf("run %d %s outside the contract", run, loadKinds[k].id)
			}
		}
	}
}

// L10: the same uplink without loss. The 2 s queue overflows and its losses come
// together, so the node reads nothing for a second and then a burst. Upload
// goodput is never above C, and a value below 0.93 C is not reported as settled;
// quic-load infers nothing held QUIC apart from TCP, as in L9.
func TestL10OverflowingUplink(t *testing.T) {
	l := newLab(t)
	l.shape(false, "10ms", "100mbit", "40ms", false)
	l.shape(true, "10ms", "10mbit", "2s", false)
	for run := range 3 {
		_, _, loads := l.check([]string{"-duration", "10s"})
		logDirections(t, run, loads)
		for k, ld := range loads {
			ceil := loadKinds[k].ceiling(10e6)
			up, m := value(ld, "upload.goodput")
			conf, _ := value(ld, "upload.goodput.confidence")
			t.Logf("run %d %s: upload %.2f Mbit/s (ceiling %.2f) confidence %v at least %v; %s", run, loadKinds[k].id, up, ceil, conf, m.AtLeast, summary(ld))
			if ld.Status == result.Error || up <= 0 || up > 1.005*ceil || conf == result.High && up < 0.93*ceil || len(held(ld)) > 0 {
				t.Errorf("run %d %s outside the contract", run, loadKinds[k].id)
			}
		}
	}
}

// Q-B1: the node's QUIC port dropped, STAMP and TCP untouched, behind a
// 100 Mbit/s link so that the network, not this host, bounds tcp-load. quic-load
// fails and names QUIC as what was blocked; tcp-load still measures.
func TestQB1QUICBlocked(t *testing.T) {
	l := newLab(t)
	l.shape(false, "10ms", "100mbit", "40ms", false)
	l.shape(true, "10ms", "100mbit", "40ms", false)
	sh(t, "ip", "netns", "exec", nodeNS, "nft", "add table inet cvacc; add chain inet cvacc in { type filter hook input priority 0; }; add rule inet cvacc in udp dport 7444 drop")
	_, _, loads := l.check([]string{"-duration", "5s", "-load-time", "5s"})
	tcpLoad, quicLoad := loads[0], loads[1]
	t.Logf("tcp-load: %s; quic-load: %s %v", summary(tcpLoad), summary(quicLoad), quicLoad.Error)
	var inferred []string
	for _, i := range quicLoad.Inferences {
		inferred = append(inferred, i.Key)
	}
	if quicLoad.Status != result.Error || !slices.Contains(inferred, "quic_load.inference.blocked") {
		t.Errorf("quic-load %s %v, want ERROR naming QUIC as blocked", quicLoad.Status, inferred)
	}
	if tcpLoad.Status == result.Error || tcpLoad.Status == result.Invalid {
		t.Errorf("tcp-load %s with only QUIC blocked", tcpLoad.Status)
	}
}

// Q-B2: an unprivileged client gets Linux's default UDP buffers. The result
// reports them with a note, its status unchanged by them, and quic-go's own
// warning never reaches stderr.
func TestQB2UnprivilegedBuffers(t *testing.T) {
	l := newLab(t)
	l.shape(false, "50ms", "100mbit", "100ms", false)
	l.shape(true, "50ms", "100mbit", "100ms", false)
	l.clientAs = []string{"setpriv", "--reuid=nobody", "--regid=nogroup", "--clear-groups"}
	_, _, loads := l.check([]string{"-duration", "5s"})
	q := loads[1]
	recv, _ := value(q, "host.udp_receive_buffer")
	var noted []string
	for _, m := range q.NotProven {
		noted = append(noted, m.Key)
	}
	t.Logf("quic-load: receive buffer %.0f B; %s; not proven %v", recv, summary(q), noted)
	if recv <= 0 || recv >= 7<<20 || !slices.Contains(noted, "quic_load.not_proven.udp_buffer") {
		t.Errorf("buffer %.0f B, notes %v: want the default buffer reported with a note", recv, noted)
	}
	if strings.Contains(l.stderr, "buffer size") {
		t.Errorf("quic-go's warning reached stderr:\n%s", l.stderr)
	}
}

// directions are a load's goodput metrics, one per direction of a phase.
var directions = []string{"download.goodput", "upload.goodput", "bidirectional.goodput.download", "bidirectional.goodput.upload"}

// senderLoss is the sender loss of the direction whose goodput is id in c,
// percent; -1 if absent.
func senderLoss(c result.Check, id string) float64 {
	v, _ := value(c, strings.Replace(id, ".goodput", ".sender_loss", 1))
	return v
}

// held lists the directions of c, by goodput metric, that quic-load inferred
// something held apart from TCP: the basis of each such inference.
func held(c result.Check) []string {
	var ids []string
	for _, m := range c.Inferences {
		if m.Key == "quic_load.inference.quic_held" && len(m.Basis) > 0 {
			ids = append(ids, m.Basis[0])
		}
	}
	return ids
}

// logDirections logs both loads of a run direction by direction.
func logDirections(t *testing.T, run int, loads []result.Check) {
	t.Helper()
	for _, id := range directions {
		tcp, _ := value(loads[0], id)
		quic, m := value(loads[1], id)
		_, tl := value(loads[0], strings.Replace(id, ".goodput", ".sender_loss", 1))
		t.Logf("run %d %s: tcp %.1f loss %.4f%% of %d, quic %.1f at least %v loss %.4f%%", run, id, tcp, senderLoss(loads[0], id), tl.Samples, quic, m.AtLeast, senderLoss(loads[1], id))
	}
	t.Logf("run %d: %s", run, summary(loads[1]))
}

// Q-E1: behind a 300 Mbit/s link with a 200 ms queue, a client held to a tenth of
// a core still fills it over TCP, not over QUIC: in each of 3 runs tcp-load's upload
// reaches 0.93 C, and quic-load infers something held QUIC and not TCP: a lower bound.
func TestQE1QUICHeldByThisHost(t *testing.T) {
	l := newLab(t, "-max-load-mb", "20000")
	l.shape(false, "10ms", "300mbit", "200ms", false)
	l.shape(true, "10ms", "300mbit", "200ms", false)
	for run := range 3 {
		_, _, loads := l.check([]string{"-duration", "10s", "-load-mb", "3000"}, "systemd-run", "--quiet", "--scope", "-p", "CPUQuota=10%")
		tcpUp, _ := value(loads[0], "upload.goodput")
		_, m := value(loads[1], "upload.goodput")
		logDirections(t, run, loads)
		if tcpUp < 0.93*ceiling(300e6) || !slices.Contains(held(loads[1]), "upload.goodput") || !m.AtLeast {
			t.Errorf("run %d outside the contract", run)
		}
	}
}

// Q-E2: bottlenecks without a queue hold both protocols alike — a 300 Mbit/s
// policer, and a token bucket with 2 ms of queue: in none of 3 runs each does
// quic-load infer that something held QUIC and not TCP.
func TestQE2PathHoldsBoth(t *testing.T) {
	for _, b := range []struct {
		name  string
		setup func(*lab)
	}{{"police", func(l *lab) { l.police(300) }}, {"shallow", func(l *lab) { l.shallow(300) }}} {
		t.Run(b.name, func(t *testing.T) {
			l := newLab(t, "-max-load-mb", "20000")
			b.setup(l)
			for run := range 3 {
				_, _, loads := l.check([]string{"-duration", "10s", "-load-mb", "3000"})
				logDirections(t, run, loads)
				if h := held(loads[1]); len(h) > 0 {
					t.Errorf("run %d: QUIC inferred held apart from TCP behind a %s bottleneck in %v", run, b.name, h)
				}
			}
		})
	}
}

// markingPath makes each direction 20 ms of delay and 100 Mbit/s with 100 ms of
// queue under fq_codel, which marks ECN instead of dropping, and has TCP ask for
// ECN: a queue that costs neither load a packet.
func (l *lab) markingPath() {
	l.shape(false, "20ms", "100mbit", "100ms", true)
	l.shape(true, "20ms", "100mbit", "100ms", true)
	sh(l.t, "ip", "netns", "exec", clientNS, "sysctl", "-q", "-w", "net.ipv4.tcp_ecn=1")
}

// dropUDP drops one in n UDP packets at random as they arrive at either end, GRO
// off there so each is one packet as sent: a path that treats UDP apart.
func (l *lab) dropUDP(n int) {
	for _, x := range []struct{ ns, ifc string }{{nodeNS, nodeIf}, {clientNS, clientIf}} {
		sh(l.t, "ip", "netns", "exec", x.ns, "ethtool", "-K", x.ifc, "gro", "off")
		sh(l.t, "tc", "-n", x.ns, "qdisc", "add", "dev", x.ifc, "handle", "ffff:", "ingress")
		sh(l.t, "tc", "-n", x.ns, "filter", "add", "dev", x.ifc, "parent", "ffff:", "protocol", "ip", "u32",
			"match", "ip", "protocol", "17", "0xff", "action", "gact", "pass", "random", "netrand", "drop", fmt.Sprint(n))
	}
}

// Q-E3: a marking queue and 5% of UDP dropped hold QUIC alone: in each of 3 runs
// quic-load infers in both directions that something held QUIC and not TCP, and
// every QUIC goodput, both directions at once too, is a lower bound.
func TestQE3PathDropsUDPAlone(t *testing.T) {
	l := newLab(t)
	l.markingPath()
	l.dropUDP(20)
	for run := range 3 {
		_, _, loads := l.check([]string{"-duration", "10s"})
		logDirections(t, run, loads)
		if h := held(loads[1]); len(h) != 2 || !goodputBounded(loads[1]) {
			t.Errorf("run %d: QUIC inferred held apart from TCP in %v; want both directions, every goodput a lower bound", run, h)
		}
	}
}

// Q-E4: the marking queue alone holds both alike: in none of 3 runs does quic-load
// infer that something held QUIC and not TCP.
func TestQE4MarkingHoldsBoth(t *testing.T) {
	l := newLab(t)
	l.markingPath()
	for run := range 3 {
		_, _, loads := l.check([]string{"-duration", "10s"})
		logDirections(t, run, loads)
		if h := held(loads[1]); len(h) > 0 {
			t.Errorf("run %d: QUIC inferred held apart from TCP behind a marking queue in %v", run, h)
		}
	}
}
