package capacity

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/i18n"
	"github.com/Lynthar/ConnVerifier/internal/protocol"
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

// fullGrant is a session that granted exactly what cfg asked for.
func fullGrant(cfg Config) runInfo {
	return runInfo{granted: protocol.Limits{
		Connections: cfg.targetConnections, DialRate: cfg.startRate, DurationS: 3600, IdleTimeoutS: 120,
	}}
}

// Rows follow the status rules of the method document, in the same order.
func TestEvaluateStatusRules(t *testing.T) {
	roomy := hostLimits{fdLimit: 1 << 20, ephemeralPorts: 28232}
	tests := []struct {
		name       string
		snap       snapshot
		ri         func(runInfo) runInfo
		status     result.Status
		errKey     string
		warnings   []string
		inferences []string
		notProven  []string
	}{
		{name: "1 tickets rejected", snap: snapshot{rejectedAuth: 3},
			status: result.Error, errKey: "rejected_auth"},
		{name: "1 every dial a host resource error", snap: snapshot{dialErrors: 3, dialErrorsHost: 3},
			status: result.Error, errKey: "no_connection_host_resource"},
		{name: "1 dials failed", snap: snapshot{dialErrors: 3, lastError: "connection refused"},
			status: result.Error, errKey: "no_connection"},
		{name: "1 no handshake finished", snap: snapshot{handshakeErrors: 2},
			status: result.Error, errKey: "handshake_failed"},
		{name: "1 node full from the start", snap: snapshot{rejectedBusy: 2},
			status: result.Invalid, warnings: []string{"warning.node_busy"}},
		{name: "1 ended before any connection", snap: snapshot{},
			status: result.Invalid, warnings: []string{"warning.ended_before_connect"}},
		{name: "2 host ran out while dialing", snap: snapshot{connects: 5, dialErrors: 1, dialErrorsHost: 1, dropClosed: 1, host: roomy},
			status: result.Invalid, warnings: []string{"warning.host_resource"}},
		{name: "2 fd limit below target", snap: snapshot{connects: 5, intervalAcks: 4, host: hostLimits{fdLimit: 10}},
			status: result.Invalid, warnings: []string{"warning.fd_limit"}},
		{name: "2 port range below target", snap: snapshot{connects: 5, intervalAcks: 4, host: hostLimits{fdLimit: 1 << 20, ephemeralPorts: 3}},
			status: result.Invalid, warnings: []string{"warning.port_range"}},
		{name: "2 node full", snap: snapshot{connects: 5, intervalAcks: 4, rejectedBusy: 1, host: roomy},
			status: result.Invalid, warnings: []string{"warning.node_busy"}},
		{name: "2 invite quota", snap: snapshot{connects: 5, intervalAcks: 4, rejectedQuota: 1, host: roomy},
			status: result.Invalid, warnings: []string{"warning.node_quota"}},
		{name: "2 granted fewer connections", snap: snapshot{connects: 2, intervalAcks: 4, host: roomy},
			ri:     func(r runInfo) runInfo { r.granted.Connections = 2; return r },
			status: result.Invalid, warnings: []string{"warning.granted_less"}},
		{name: "3 closed by peer and by node", snap: snapshot{connects: 5, intervalAcks: 4, dropClosed: 2, dropNodeClosed: 1, nodeClosedIdle: 1, host: roomy},
			status: result.Warn, inferences: []string{"inference.peer_closed", "inference.node_closed"}},
		{name: "3 silent drop", snap: snapshot{connects: 5, intervalAcks: 4, dropTimeouts: 1, host: roomy},
			status: result.Warn, inferences: []string{"inference.silent_drops"}, notProven: []string{"not_proven.lifetime_upper_bound"}},
		{name: "4 dial rate lowered", snap: snapshot{connects: 5, intervalAcks: 4, host: roomy},
			ri:     func(r runInfo) runInfo { r.granted.DialRate = 1; return r },
			status: result.Warn, warnings: []string{"warning.dial_rate_cut"}},
		{name: "4 run cut by the session", snap: snapshot{connects: 5, intervalAcks: 4, host: roomy},
			ri:     func(r runInfo) runInfo { r.nodeCut = true; return r },
			status: result.Warn, warnings: []string{"warning.duration_cut"}},
		{name: "5 no full interval", snap: snapshot{connects: 5, host: roomy},
			status: result.Warn, notProven: []string{"not_proven.no_full_interval"}},
		{name: "6 handshake errors alone do not lift the status", snap: snapshot{connects: 5, intervalAcks: 4, handshakeErrors: 1, host: roomy},
			status: result.Pass, notProven: []string{"not_proven.handshake_errors"}},
		{name: "6 clean run", snap: snapshot{connects: 5, intervalAcks: 4, host: roomy},
			status: result.Pass},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validClientConfig()
			cfg.targetConnections, cfg.startRate = 5, 10
			ri := fullGrant(cfg)
			if tt.ri != nil {
				ri = tt.ri(ri)
			}
			var c result.Check
			evaluate(&c, tt.snap, cfg, ri)
			if c.Status != tt.status {
				t.Fatalf("status = %s, want %s (warnings %v)", c.Status, tt.status, keys(c.Warnings))
			}
			gotErr := ""
			if c.Error != nil {
				gotErr = strings.TrimPrefix(c.Error.Key, "tcp_capacity.error.")
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

// Before any connection: no session, or one the run cannot use.
func TestPreRunOutcomes(t *testing.T) {
	cfg := validClientConfig()
	tests := []struct {
		name string
		err  *sessionError
		key  string
	}{
		{"wrong key", &sessionError{kind: "identity", err: protocol.ErrPinMismatch}, "tcp_capacity.error.node_identity"},
		{"unreachable", &sessionError{kind: "unreachable", err: errors.New("connection refused")}, "tcp_capacity.error.node_unreachable"},
		{"refused", &sessionError{kind: "refused", refusal: &protocol.ErrorResponse{Reason: "auth"}}, "tcp_capacity.error.session_refused"},
		{"busy", &sessionError{kind: "busy", refusal: &protocol.ErrorResponse{Reason: "busy", RetryAfterS: 60}}, "tcp_capacity.error.node_busy"},
	}
	for _, tt := range tests {
		c := newCheck(cfg, "192.0.2.10:7443", "test")
		sessionFailed(&c, tt.err)
		if c.Status != result.Error || c.Error == nil || c.Error.Key != tt.key {
			t.Errorf("%s: status %s error %+v, want ERROR %s", tt.name, c.Status, c.Error, tt.key)
		}
	}
	c := newCheck(cfg, "192.0.2.10:7443", "test")
	idleTooShort(&c, protocol.Limits{IdleTimeoutS: 1}, 2*time.Second)
	if c.Status != result.Invalid || keys(c.Warnings)[0] != "warning.idle_too_short" {
		t.Errorf("idle too short: status %s warnings %v", c.Status, keys(c.Warnings))
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
		for range tt.samples {
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

// goldenRun is a synthetic run: documentation-range addresses, made-up numbers.
func goldenRun() result.Run {
	h := newRttHistogram()
	for i := range 40 {
		h.Record(time.Duration(100+5*i) * time.Microsecond)
	}
	counts, total := h.collect()

	cfg := validClientConfig()
	cfg.targetConnections, cfg.startRate = 50, 100
	cfg.heartbeat = 30 * time.Second
	cfg.duration = 10 * time.Minute
	snap := snapshot{
		dialAttempts: 75, connects: 73,
		dropTimeouts: 3, dropClosed: 1, dropErrors: 2, dropNodeClosed: 25, nodeClosedIdle: 25,
		heartbeatsSent: 233, heartbeatsAck: 200, intervalAcks: 120,
		sessionNanos: uint64(31 * 45 * time.Second), family: "ipv4",
		host: hostLimits{fdLimit: 10240, ephemeralPorts: 16384},
	}
	c := newCheck(cfg, "192.0.2.10:7443", "tokyo-test")
	c.Node.Version = "v0.0.0-test"
	c.Node.ObservedAddr = "198.51.100.7:51234"
	c.Node.Granted = &result.Grant{Connections: 50, DialRate: 100, DurationS: 620, IdleTimeoutS: 120}
	c.Node.LoadStart = &result.NodeLoad{Sessions: 1, Connections: 0, MaxConnections: 20000, UptimeS: 86400}
	c.Node.LoadEnd = &result.NodeLoad{Sessions: 0, Connections: 0, MaxConnections: 20000, UptimeS: 87000}
	ri := runInfo{granted: protocol.Limits{Connections: 50, DialRate: 100, DurationS: 620, IdleTimeoutS: 120}}
	finishCheck(&c, cfg, finalStats{snap: snap, rttCounts: counts, rttTotal: total}, h, ri)
	c.ElapsedMs = 600012

	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return result.Run{
		Schema:    result.Schema,
		Tool:      result.Tool{Name: "connverifier", Version: "v0.0.0-test", Go: "go1.x", OS: "linux", Arch: "amd64"},
		RunID:     "TESTRUNID",
		Started:   started,
		Ended:     started.Add(10*time.Minute + 12*time.Millisecond),
		ElapsedMs: 600012,
		Checks:    []result.Check{c},
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

// Every message any rule can produce must resolve in every catalog; a missing key
// renders as itself and would show up here.
func TestTextHasNoRawKeys(t *testing.T) {
	cfg := validClientConfig()
	cfg.targetConnections, cfg.startRate = 5, 10
	all := fullGrant(cfg)
	cut := all
	cut.granted.Connections, cut.granted.DialRate, cut.nodeCut = 2, 1, true
	runs := []struct {
		s  snapshot
		ri runInfo
	}{
		{snapshot{rejectedAuth: 1}, all},
		{snapshot{dialErrors: 3, lastError: "refused"}, all},
		{snapshot{dialErrors: 3, dialErrorsHost: 3, lastError: "too many open files"}, all},
		{snapshot{handshakeErrors: 1, lastError: "EOF"}, all},
		{snapshot{rejectedBusy: 1}, all},
		{snapshot{}, all},
		{snapshot{connects: 5, dialErrors: 1, dialErrorsHost: 1, rejectedBusy: 1, rejectedQuota: 1, host: hostLimits{fdLimit: 10, ephemeralPorts: 1}}, cut},
		{snapshot{connects: 5, handshakeErrors: 1, dropTimeouts: 1, dropClosed: 1, dropErrors: 1, dropBadAcks: 1, dropNodeClosed: 3, nodeClosedIdle: 1, nodeClosedSession: 1, nodeClosedOther: 1}, all},
		{snapshot{connects: 5}, runInfo{granted: protocol.Limits{Connections: 5, DialRate: 1, DurationS: 60, IdleTimeoutS: 60}, nodeCut: true}},
	}
	run := goldenRun()
	for _, r := range runs {
		c := newCheck(cfg, "192.0.2.10:7443", "test")
		finishCheck(&c, cfg, finalStats{snap: r.s}, newRttHistogram(), r.ri)
		run.Checks = append(run.Checks, c)
	}
	for _, e := range []*sessionError{
		{kind: "identity", err: protocol.ErrPinMismatch},
		{kind: "unreachable", err: errors.New("refused")},
		{kind: "refused", refusal: &protocol.ErrorResponse{Reason: "auth", Message: "m"}},
		{kind: "busy", refusal: &protocol.ErrorResponse{Reason: "busy", RetryAfterS: 60}},
	} {
		c := newCheck(cfg, "192.0.2.10:7443", "test")
		sessionFailed(&c, e)
		run.Checks = append(run.Checks, c)
	}
	idle := newCheck(cfg, "192.0.2.10:7443", "test")
	idleTooShort(&idle, protocol.Limits{IdleTimeoutS: 1}, 2*time.Second)
	run.Checks = append(run.Checks, idle)

	rawKey := regexp.MustCompile(`\b(tcp_capacity|metric|param|label|section|status|check|line|value|confirm|node)\.[A-Za-z_]`)
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
		if loc := rawKey.FindStringIndex(b.String()); loc != nil {
			t.Errorf("%s: unresolved key near %q", lang, b.String()[loc[0]:min(b.Len(), loc[0]+60)])
		}
	}
}
