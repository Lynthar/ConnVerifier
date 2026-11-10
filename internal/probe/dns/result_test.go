package dns

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

// syntheticAnswers is n made-up answers around base; every 10th is lost when lose,
// and the third carries rcode.
func syntheticAnswers(n int, base time.Duration, lose bool, rcode int) outcome {
	o := outcome{answers: make([]answer, n)}
	for i := range o.answers {
		o.answers[i].sent = true
		if lose && i%10 == 9 {
			continue
		}
		o.answers[i].ok, o.answers[i].rtt = true, base+time.Duration(i%5)*311*time.Microsecond
		if i == 2 {
			o.answers[i].rcode = rcode
		}
	}
	return o
}

func resolverCheck(family string, index int, class string) result.Check {
	c := newCheck(family)
	c.Params["resolver"], c.Params["resolver_class"] = index, class
	return c
}

func goldenRun() result.Run {
	v4 := resolverCheck("ipv4", 1, "private")
	finish(&v4, syntheticAnswers(queries, 3*time.Millisecond, false, 0), false)
	v4.ElapsedMs = 1_906
	v6 := resolverCheck("ipv6", 2, "link_local")
	finish(&v6, syntheticAnswers(queries, 4*time.Millisecond, true, 2), false)
	v6.ElapsedMs = 3_902
	return result.Run{
		Schema: result.Schema, Tool: result.Tool{Name: "connverifier", Version: "v0.0.0-golden", Go: "go1.27.0", OS: "linux", Arch: "amd64"},
		RunID: "golden", Started: t0, Ended: t0.Add(5808 * time.Millisecond), ElapsedMs: 5_808,
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
	for _, class := range []string{"loopback", "link_local", "private", "shared", "benchmarking", "public", "other"} {
		c := resolverCheck("ipv4", 1, class)
		c.NotProven = append(c.NotProven, msg("dns.not_proven.tunnel", nil))
		finish(&c, syntheticAnswers(queries, time.Millisecond, true, 5), false)
		checks = append(checks, c)
	}
	unreachable := resolverCheck("ipv6", 1, "public")
	finish(&unreachable, outcome{answers: []answer{{sent: true}}, sendErr: errors.New("network is unreachable")}, false)
	interrupted := resolverCheck("ipv4", 1, "private")
	finish(&interrupted, outcome{}, true)
	skipped := newCheck("ipv6")
	skipped.Status, skipped.Warnings = result.Skip, []result.Message{msg("dns.skip.no_resolver", map[string]any{"family": "ipv6"})}
	unsupported := newCheck("ipv4")
	unsupported.Status, unsupported.Warnings = result.Unsupported, []result.Message{msg("dns.unsupported.no_config", map[string]any{"error": "x"})}
	checks = append(checks, unreachable, interrupted, skipped, unsupported)

	run := result.Run{Checks: checks}
	rawKey := regexp.MustCompile(`\b(dns|class|metric|param|label|section|status|check|line|value)\.[A-Za-z_]`)
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
