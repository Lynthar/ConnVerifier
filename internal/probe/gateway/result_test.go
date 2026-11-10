package gateway

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
	"github.com/Lynthar/ConnVerifier/internal/report"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

var update = flag.Bool("update", false, "rewrite golden files")

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// syntheticReplies is n made-up round trips around base, every 25th lost.
func syntheticReplies(n int, base time.Duration, lose bool) []reply {
	rs := make([]reply, n)
	for i := range rs {
		if lose && i%25 == 24 {
			continue
		}
		rs[i] = reply{rtt: base + time.Duration(i%7)*137*time.Microsecond, ok: true}
	}
	return rs
}

func goldenRun() result.Run {
	v4 := newCheck("ipv4")
	v4.Params["gateway_class"] = "private"
	finish(&v4, outcome{protocol: "icmp", replies: syntheticReplies(probes, 1200*time.Microsecond, true)}, false)
	v4.ElapsedMs = 5_101
	v6 := newCheck("ipv6")
	v6.Params["gateway_class"] = "link_local"
	finish(&v6, outcome{protocol: "tcp", port: 53, replies: syntheticReplies(probes, 1500*time.Microsecond, false)}, false)
	v6.ElapsedMs = 10_904
	return result.Run{
		Schema: result.Schema, Tool: result.Tool{Name: "connverifier", Version: "v0.0.0-golden", Go: "go1.27.0", OS: "linux", Arch: "amd64"},
		RunID: "golden", Started: t0, Ended: t0.Add(16005 * time.Millisecond), ElapsedMs: 16_005,
		Checks: []result.Check{v4, v6},
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
	var checks []result.Check
	add := func(f func(*result.Check)) {
		c := newCheck("ipv4")
		f(&c)
		checks = append(checks, c)
	}
	for _, class := range []string{"loopback", "link_local", "private", "shared", "benchmarking", "public", "other"} {
		add(func(c *result.Check) {
			c.Params["gateway_class"] = class
			finish(c, outcome{protocol: "icmp", replies: syntheticReplies(probes, time.Millisecond, true)}, false)
		})
	}
	noICMP := errors.New("permission denied")
	add(func(c *result.Check) {
		finish(c, outcome{protocol: "tcp", port: 80, icmpErr: noICMP, replies: syntheticReplies(probes, time.Millisecond, true)}, false)
	})
	add(func(c *result.Check) { finish(c, outcome{}, false) })
	add(func(c *result.Check) { finish(c, outcome{icmpErr: noICMP}, false) })
	add(func(c *result.Check) { finish(c, outcome{}, true) })
	add(func(c *result.Check) { skip(c, "gateway.skip.no_default_route") })
	add(func(c *result.Check) { skip(c, "gateway.skip.no_gateway") })
	add(func(c *result.Check) {
		c.Status = result.Unsupported
		c.Warnings = append(c.Warnings, msg("gateway.unsupported.no_routes", map[string]any{"error": "x"}))
	})
	run := result.Run{Checks: checks}
	rawKey := regexp.MustCompile(`\b(gateway|class|metric|param|label|section|status|check|line|value)\.[A-Za-z_]`)
	for _, lang := range i18n.Supported {
		cat, err := i18n.Load(lang)
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if err := report.Text(&b, run, cat); err != nil {
			t.Fatal(err)
		}
		if m := rawKey.FindString(b.String()); m != "" {
			t.Errorf("%s text contains raw key %q", lang, m)
		}
		for _, raw := range []string{"ipv4", "ipv6", "link_local"} {
			if bytes.Contains(b.Bytes(), []byte(raw)) {
				t.Errorf("%s text shows the identifier %q untranslated", lang, raw)
			}
		}
	}
}
