//go:build accuracy && linux

// Package accuracy checks measured values against conditions injected with
// netem between two network namespaces. It needs root, iproute2 and ping, runs
// for about forty minutes, and is not part of the commit gate.
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

func newLab(t *testing.T) *lab {
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
	}
	state := filepath.Join(t.TempDir(), "node")
	cmd := exec.Command(bin, "invite", "create", "-state-dir", state, "-label", "acc", "-addr", nodeIP+":7443")
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

// check runs connverifier check from the client namespace and returns the UDP
// and TCP results; prefix wraps the command (such as a CPU-limited scope).
func (l *lab) check(duration string, prefix ...string) (udp, tcp result.Check) {
	l.t.Helper()
	args := append(prefix, "ip", "netns", "exec", clientNS, bin, "check", "-format", "json", "-duration", duration)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "CONNVERIFIER_NODE="+l.invite)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Run()
	var run result.Run
	if err := json.Unmarshal(stdout.Bytes(), &run); err != nil || len(run.Checks) != 2 {
		l.t.Fatalf("check output: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	return run.Checks[0], run.Checks[1]
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
		udp, tcp := l.check("20s")
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
		udp, tcp := l.check("30s")
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
		udp, _ := l.check("20s")
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
	udp, _ := l.check("20s")
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
	udp, tcp := l.check("20s", "systemd-run", "--quiet", "--scope", "-p", "CPUQuota=1%")
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
	udp, tcp := l.check("20s")
	for _, id := range []string{"lost", "duplicates.forward", "duplicates.return", "reordered"} {
		if v, _ := value(udp, id); v != 0 {
			t.Errorf("%s = %v on a clean path", id, v)
		}
	}
	if udp.Status != result.Pass || tcp.Status != result.Pass {
		t.Errorf("udp %s, tcp %s", udp.Status, tcp.Status)
	}
}
