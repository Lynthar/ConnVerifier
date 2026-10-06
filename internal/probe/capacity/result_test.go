package capacity

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/i18n"
	"github.com/Lynthar/ConnVerifier/internal/report"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/")

func keys(msgs []result.Message) []string {
	var k []string
	for _, m := range msgs {
		k = append(k, strings.TrimPrefix(m.Key, "tcp_capacity."))
	}
	return k
}

// Rows follow the status rules of the method document, in the same order.
func TestEvaluateStatusRules(t *testing.T) {
	tests := []struct {
		name       string
		snap       snapshot
		target     int
		status     result.Status
		errKey     string
		warnings   []string
		inferences []string
		notProven  []string
	}{
		{
			name:   "1 no connects after failed dials",
			snap:   snapshot{dialErrors: 3, lastDialError: "connect: connection refused"},
			status: result.Error, errKey: "tcp_capacity.error.no_connection",
		},
		{
			name:   "1 every failed dial was a host resource error",
			snap:   snapshot{dialErrors: 3, dialErrorsHost: 3},
			status: result.Error, errKey: "tcp_capacity.error.no_connection_host_resource",
		},
		{
			name:     "2 no connects and no dial errors",
			snap:     snapshot{},
			status:   result.Invalid,
			warnings: []string{"warning.ended_before_connect"},
		},
		{
			name:     "3 host ran out of resources while dialing",
			snap:     snapshot{connects: 5, dialErrors: 2, dialErrorsHost: 2, dropClosed: 1},
			status:   result.Invalid,
			warnings: []string{"warning.host_resource"},
		},
		{
			name:     "3 fd limit below target",
			snap:     snapshot{connects: 5, intervalAcks: 4, host: hostLimits{fdLimit: 100}},
			target:   200,
			status:   result.Invalid,
			warnings: []string{"warning.fd_limit"},
		},
		{
			name:     "3 port range below target",
			snap:     snapshot{connects: 5, intervalAcks: 4, host: hostLimits{fdLimit: 1 << 20, ephemeralPorts: 100}},
			target:   200,
			status:   result.Invalid,
			warnings: []string{"warning.port_range"},
		},
		{
			name:       "4 drops after a full interval",
			snap:       snapshot{connects: 5, dropClosed: 2, intervalAcks: 4},
			status:     result.Warn,
			inferences: []string{"inference.peer_closed"},
			notProven:  []string{"not_proven.rejection_vs_drop", "not_proven.lifetime_upper_bound"},
		},
		{
			name:      "5 no drops but no full interval",
			snap:      snapshot{connects: 5},
			status:    result.Warn,
			notProven: []string{"not_proven.no_full_interval"},
		},
		{
			name:       "4 and 5 together",
			snap:       snapshot{connects: 5, dropTimeouts: 1, dropBadAcks: 1},
			status:     result.Warn,
			inferences: []string{"inference.silent_drops", "inference.bad_ack"},
			notProven:  []string{"not_proven.rejection_vs_drop", "not_proven.lifetime_upper_bound", "not_proven.no_full_interval"},
		},
		{
			name:   "6 no drops and a full interval within host limits",
			snap:   snapshot{connects: 5, intervalAcks: 4, host: hostLimits{fdLimit: 1 << 20, ephemeralPorts: 16384}},
			target: 200,
			status: result.Pass,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validClientConfig()
			cfg.targetConnections = max(1, tt.target)
			var c result.Check
			evaluate(&c, tt.snap, cfg)
			if c.Status != tt.status {
				t.Fatalf("status = %s, want %s", c.Status, tt.status)
			}
			gotErr := ""
			if c.Error != nil {
				gotErr = c.Error.Key
			}
			if gotErr != tt.errKey {
				t.Fatalf("error key = %q, want %q", gotErr, tt.errKey)
			}
			for _, field := range []struct {
				name      string
				got, want []string
			}{
				{"warnings", keys(c.Warnings), tt.warnings},
				{"inferences", keys(c.Inferences), tt.inferences},
				{"not proven", keys(c.NotProven), tt.notProven},
			} {
				if !reflect.DeepEqual(field.got, field.want) {
					t.Fatalf("%s = %v, want %v", field.name, field.got, field.want)
				}
			}
		})
	}
}

func TestQuantileSufficiency(t *testing.T) {
	tests := []struct {
		samples      int
		p            int
		insufficient bool
	}{
		{1, 50, true}, {2, 50, false},
		{19, 95, true}, {20, 95, false},
		{99, 99, true}, {100, 99, false},
	}
	for _, tt := range tests {
		h := newRttHistogram()
		for i := 0; i < tt.samples; i++ {
			h.Record(time.Millisecond)
		}
		counts, total := h.collect()
		m := quantileMetric(h, counts, total, tt.p)
		if m.Insufficient != tt.insufficient || (m.Value == nil) != tt.insufficient {
			t.Errorf("p%d with %d samples: insufficient=%v value=%v, want insufficient=%v",
				tt.p, tt.samples, m.Insufficient, m.Value, tt.insufficient)
		}
	}
}

// goldenRun is a synthetic run: documentation-range address, made-up counts.
func goldenRun() result.Run {
	h := newRttHistogram()
	for i := 0; i < 40; i++ {
		h.Record(time.Duration(100+5*i) * time.Microsecond)
	}
	counts, total := h.collect()

	cfg := validClientConfig()
	cfg.address = "192.0.2.10:9000"
	cfg.targetConnections = 50
	cfg.heartbeat = 30 * time.Second
	cfg.duration = 10 * time.Minute
	snap := snapshot{
		dialAttempts: 75, connects: 73, dialErrors: 2,
		dropTimeouts: 3, dropClosed: 1, dropErrors: 27,
		heartbeatsSent: 233, heartbeatsAck: 200, intervalAcks: 120,
		sessionNanos: uint64(31 * 45 * time.Second), family: "ipv4",
		host: hostLimits{fdLimit: 10240, ephemeralPorts: 16384},
	}
	check := buildCheck(cfg, finalStats{snap: snap, rttCounts: counts, rttTotal: total}, h, 10*time.Minute+12*time.Millisecond)

	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return result.Run{
		Schema:    result.Schema,
		Tool:      result.Tool{Name: "connverifier", Version: "v0.0.0-test", Go: "go1.x", OS: "linux", Arch: "amd64"},
		RunID:     "TESTRUNID",
		Started:   started,
		Ended:     started.Add(10*time.Minute + 12*time.Millisecond),
		ElapsedMs: 600012,
		Checks:    []result.Check{check},
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

// Every message the status rules can produce must resolve in every catalog; a
// missing key renders as itself and would show up here.
func TestTextHasNoRawKeys(t *testing.T) {
	snaps := []snapshot{
		{dialErrors: 3, lastDialError: "refused"},
		{dialErrors: 3, dialErrorsHost: 3, lastDialError: "too many open files"},
		{connects: 5, dialErrors: 1, dialErrorsHost: 1, host: hostLimits{fdLimit: 10, ephemeralPorts: 1}},
		{},
		{connects: 5, dropTimeouts: 1, dropClosed: 1, dropErrors: 1, dropBadAcks: 1},
		{connects: 5, intervalAcks: 1},
	}
	run := goldenRun()
	for _, s := range snaps {
		h := newRttHistogram()
		run.Checks = append(run.Checks, buildCheck(validClientConfig(), finalStats{snap: s}, h, time.Second))
	}
	prefixes := []string{"tcp_capacity.", "metric.", "param.", "label.", "section.", "status.", "check.", "line.", "value.", "confirm."}
	untilStopped, timed := validClientConfig(), validClientConfig()
	untilStopped.duration, timed.duration = 0, time.Minute
	for _, lang := range i18n.Supported {
		cat, err := i18n.Load(lang)
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if err := report.Text(&b, run, cat); err != nil {
			t.Fatal(err)
		}
		for _, cfg := range []Config{untilStopped, timed} {
			b.WriteString(report.Message(cat, cfg.Notice()) + "\n")
		}
		for _, key := range []string{"confirm.prompt", "confirm.declined", "confirm.needs_yes"} {
			b.WriteString(cat.Text(key, nil) + "\n")
		}
		for _, p := range prefixes {
			if i := strings.Index(b.String(), p); i >= 0 {
				t.Errorf("%s: unresolved key near %q", lang, b.String()[i:min(len(b.String()), i+60)])
			}
		}
	}
}
