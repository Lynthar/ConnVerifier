package baseline

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
	"github.com/Lynthar/ConnVerifier/internal/protocol"
	"github.com/Lynthar/ConnVerifier/internal/report"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

var update = flag.Bool("update", false, "rewrite golden files")

// syntheticProbes is n probes 20 ms apart with made-up delays: every 50th lost,
// one late, one duplicated on the way back and one pair swapped.
func syntheticProbes(n int) (probes []probe, arrivals []int) {
	for i := range n {
		sent := t0.Add(time.Duration(i) * 20 * time.Millisecond)
		p := probe{planned: sent, sent: sent}
		rtt := 30*time.Millisecond + time.Duration(i%9)*time.Millisecond
		switch {
		case i%50 == 49:
		case i == 100:
			p.replies = []reply{{at: sent.Add(4 * time.Second), reflSeq: uint32(i)}}
		default:
			p.replies = []reply{{at: sent.Add(rtt), reflSeq: uint32(i), residence: 40 * time.Microsecond}}
		}
		if i == 200 {
			p.replies = append(p.replies, p.replies[0])
		}
		probes = append(probes, p)
	}
	for i := range n {
		if len(probes[i].replies) > 0 && i != 100 {
			arrivals = append(arrivals, i)
		}
	}
	arrivals[10], arrivals[11] = arrivals[11], arrivals[10]
	return probes, arrivals
}

func syntheticUDP(n int, sendErrors uint64) *udpRun {
	r := &udpRun{end: t0.Add(time.Minute), sendErrors: sendErrors}
	r.probes, r.arrivals = syntheticProbes(n)
	return r
}

func syntheticTCP(n int) *tcpRun {
	r := &tcpRun{end: t0.Add(time.Minute)}
	r.probes, r.arrivals = syntheticProbes(n)
	return r
}

func goldenRun() result.Run {
	cfg := Config{rate: 50, duration: 20 * time.Second, tmax: defaultTmax}
	node := &result.NodeReport{
		Label: "example", Version: "v0.0.0-golden", ObservedAddr: "198.51.100.7:51234",
		Granted:   &result.Grant{Connections: 1, DialRate: 1, DurationS: 36, IdleTimeoutS: 60, StampRate: 100},
		LoadStart: &result.NodeLoad{Sessions: 1, MaxConnections: 20000, UptimeS: 3600},
		LoadEnd:   &result.NodeLoad{MaxConnections: 20000, UptimeS: 3624},
	}
	udp := newCheck("udp-baseline", "udp", cfg, "192.0.2.10:7443", "example", udpPacketLen)
	udp.Node = node
	finishUDP(&udp, syntheticUDP(1000, 0), &protocol.StampCounts{Received: 990}, defaultTmax, defaultSlipLimit)
	tcp := newCheck("tcp-baseline", "tcp", cfg, "192.0.2.10:7443", "example", tcpPacketLen)
	tcp.Node = node
	finishTCP(&tcp, syntheticTCP(1000), defaultTmax, defaultSlipLimit)
	udp.ElapsedMs, tcp.ElapsedMs = 23_100, 23_100
	return result.Run{
		Schema: result.Schema, Tool: result.Tool{Name: "connverifier", Version: "v0.0.0-golden", Go: "go1.27.0", OS: "linux", Arch: "amd64"},
		RunID: "golden", Started: t0, Ended: t0.Add(23100 * time.Millisecond), ElapsedMs: 23_100,
		Checks: []result.Check{udp, tcp},
	}
}

func TestResultGolden(t *testing.T) {
	run := goldenRun()
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
	cfg := Config{rate: 50, duration: 20 * time.Second, tmax: defaultTmax}
	var checks []result.Check
	add := func(id string, f func(*result.Check)) {
		c := newCheck(id, "udp", cfg, "192.0.2.10:7443", "example", udpPacketLen)
		f(&c)
		checks = append(checks, c)
	}
	node := &protocol.StampCounts{Received: 1000}
	for _, o := range []outcome{{}, {sent: 10}, {sent: 1000, received: 900, slipped: 20, slowNode: 20}, {sent: 1000, received: 999, dupForward: 1, dupReturn: 1, reordered: 1}} {
		for _, counts := range []*protocol.StampCounts{nil, {}, node, {Received: 1, OverRate: 1, OtherAddr: 1}} {
			add("udp-baseline", func(c *result.Check) { evaluateUDP(c, o, counts) })
		}
		for _, broke := range []error{nil, errors.New("reset")} {
			add("tcp-baseline", func(c *result.Check) { evaluateTCP(c, o, broke) })
		}
	}
	add("udp-baseline", func(c *result.Check) {
		finishUDP(c, syntheticUDP(300, 1), &protocol.StampCounts{Received: 280}, defaultTmax, defaultSlipLimit)
	})
	add("udp-baseline", func(c *result.Check) { finishUDP(c, syntheticUDP(300, 0), nil, defaultTmax, defaultSlipLimit) })
	add("udp-baseline", func(c *result.Check) { unsupported(c, "udp_baseline.unsupported.invite_without_udp") })
	add("tcp-baseline", interruptedBeforeData)
	add("udp-baseline", func(c *result.Check) { unsupported(c, "udp_baseline.unsupported.node_without_stamp") })
	add("udp-baseline", func(c *result.Check) {
		notRun(c, "baseline.warning.rate_not_granted", map[string]any{"granted": 10, "asked": 50})
	})
	add("tcp-baseline", func(c *result.Check) {
		notRun(c, "baseline.warning.duration_not_granted", map[string]any{"granted_ms": 5000})
	})
	add("udp-baseline", func(c *result.Check) {
		c.Status, c.Error = result.Error, &result.Message{Key: "udp_baseline.error.no_socket", Params: map[string]any{"error": "x"}}
	})
	for _, f := range []dataFailure{{kind: "reject", reject: protocol.Frame{Reason: protocol.ReasonQuota}}, {kind: "reject"}, {kind: "dial", err: errors.New("x")}, {kind: "handshake", err: errors.New("x")}} {
		add("tcp-baseline", func(c *result.Check) { tcpFailed(c, &f) })
	}
	run := result.Run{Checks: checks}
	rawKey := regexp.MustCompile(`\b(udp_baseline|tcp_baseline|baseline|session|metric|param|label|section|status|check|line|value|node)\.[A-Za-z_]`)
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
