package report

import (
	"strings"
	"testing"

	"github.com/Lynthar/ConnVerifier/internal/i18n"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

// Every interface kind renders in every catalog, the host section comes before the
// first check, and a note shared by two families is printed once.
func TestTextHost(t *testing.T) {
	var ifs []result.Interface
	for _, kind := range []string{"wifi", "wired", "tunnel", "ppp", "other", "unknown"} {
		ifs = append(ifs, result.Interface{Family: "ipv4", Kind: kind})
	}
	ifs = append(ifs, result.Interface{Family: "ipv6", Kind: "wifi"}, result.Interface{Kind: "unknown"})
	run := result.Run{
		Host:   &result.Host{Interfaces: ifs},
		Checks: []result.Check{{ID: "udp-baseline", Status: result.Skip, Warnings: []result.Message{{Key: "check.not_selected"}}}},
	}
	for _, lang := range i18n.Supported {
		cat, err := i18n.Load(lang)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		if err := Text(&b, run, cat); err != nil {
			t.Fatal(err)
		}
		out := b.String()
		if strings.Contains(out, "host.") {
			t.Errorf("%s: raw key in\n%s", lang, out)
		}
		if !strings.HasPrefix(out, cat.Text("section.host", nil)+"\n") || !strings.Contains(out, "\n\n"+cat.Text("check.udp-baseline.title", nil)) {
			t.Errorf("%s: host section is not first, apart from the checks:\n%s", lang, out)
		}
		if n := strings.Count(out, cat.Text("host.note.wifi", nil)); n != 1 {
			t.Errorf("%s: Wi-Fi note printed %d times, want once:\n%s", lang, n, out)
		}
	}
}

func TestFormatMs(t *testing.T) {
	tests := []struct {
		ms   float64
		want string
	}{
		{0.0005, "0.5 µs"},
		{0.131, "131 µs"},
		{1.5, "1.5 ms"},
		{12.34, "12.3 ms"},
		{255, "255 ms"},
		{999.6, "1000 ms"},
		{1000, "1 s"},
		{30000, "30 s"},
		{60000, "1 min"},
		{90000, "1.5 min"},
	}
	for _, tt := range tests {
		if got := formatMs(tt.ms); got != tt.want {
			t.Errorf("formatMs(%v) = %q, want %q", tt.ms, got, tt.want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[float64]string{
		0: "0 B", 999: "999 B", 1500: "1.5 kB", 500e6: "500 MB", 1.5e9: "1.5 GB", 20e9: "20 GB",
	} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%v) = %q, want %q", n, got, want)
		}
	}
}
