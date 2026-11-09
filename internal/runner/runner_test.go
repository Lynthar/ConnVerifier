package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Lynthar/ConnVerifier/internal/i18n"
	"github.com/Lynthar/ConnVerifier/internal/probe/baseline"
	"github.com/Lynthar/ConnVerifier/internal/probe/load"
	"github.com/Lynthar/ConnVerifier/internal/probe/nodeclient"
	"github.com/Lynthar/ConnVerifier/internal/protocol"
	"github.com/Lynthar/ConnVerifier/internal/report"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

func TestConfirm(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		interactive bool
		yes         bool
		want        error
		asked       bool
	}{
		{"yes flag skips the question", "", false, true, nil, false},
		{"interactive y", "y\n", true, false, nil, true},
		{"interactive yes in capitals", "YES\n", true, false, nil, true},
		{"interactive no", "n\n", true, false, ErrDeclined, true},
		{"interactive empty answer", "\n", true, false, ErrDeclined, true},
		{"interactive end of input", "", true, false, ErrDeclined, true},
		{"no terminal and no yes flag", "y\n", false, false, ErrNeedsYes, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			err := Confirm(strings.NewReader(tt.input), &out, tt.interactive, tt.yes, "NOTICE", "PROMPT")
			if !errors.Is(err, tt.want) {
				t.Fatalf("Confirm = %v, want %v", err, tt.want)
			}
			if !strings.Contains(out.String(), "NOTICE") {
				t.Fatalf("notice not shown: %q", out.String())
			}
			if asked := strings.Contains(out.String(), "PROMPT"); asked != tt.asked {
				t.Fatalf("prompt shown = %v, want %v", asked, tt.asked)
			}
		})
	}
}

func TestSelection(t *testing.T) {
	t.Setenv(nodeclient.InviteEnv, "")
	invite := protocol.Invite{Label: "example", Addrs: []string{"192.0.2.10:7443"}}.Encode()
	tests := []struct {
		only, node string
		valid      bool
		runs       []string
		loads      int
	}{
		{"", invite, true, checkIDs, 2},
		{"udp-baseline", invite, true, []string{"udp-baseline"}, 0},
		{" tcp-load , quic-load ", invite, true, []string{"tcp-load", "quic-load"}, 2},
		{"quic-load,", invite, true, []string{"quic-load"}, 1},
		{"tcp-load,dns", invite, false, nil, 0},
		{"TCP-load", invite, false, nil, 0},
		{"", "", true, checkIDs, 0},
		{"gateway,tcp-load", "", true, []string{"gateway", "tcp-load"}, 0},
		{"udp-baseline,tcp-load", "", false, nil, 0},
		{"gateway", "cvi1_not-an-invite", true, []string{"gateway"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.only+"/"+tt.node, func(t *testing.T) {
			cfg := Config{only: tt.only}
			if err := cfg.Validate(tt.node); (err == nil) != tt.valid {
				t.Fatalf("Validate = %v, want valid %v", err, tt.valid)
			}
			if !tt.valid {
				return
			}
			var runs []string
			for _, id := range checkIDs {
				if cfg.Selected(id) {
					runs = append(runs, id)
				}
			}
			if !slices.Equal(runs, tt.runs) || cfg.Loads(tt.node) != tt.loads {
				t.Fatalf("runs %v and %d loads, want %v and %d", runs, cfg.Loads(tt.node), tt.runs, tt.loads)
			}
		})
	}
}

// Without an invite, check runs what needs no node and says why the rest did not.
func TestCheckWithoutInvite(t *testing.T) {
	t.Setenv(nodeclient.InviteEnv, "")
	checks, err := Check(t.Context(), Config{only: "udp-baseline,tcp-load"}, baseline.Config{}, load.Config{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range checks {
		if c.Status != result.Skip || len(c.Warnings) != 1 {
			t.Fatalf("%s: %s %v, want SKIP with one reason", c.ID, c.Status, c.Warnings)
		}
		got = append(got, c.ID+":"+c.Warnings[0].Key)
	}
	want := []string{
		"gateway:check.not_selected", "gateway:check.not_selected",
		"udp-baseline:check.no_invite", "tcp-baseline:check.no_invite",
		"tcp-load:check.no_invite", "quic-load:check.not_selected",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("checks %v, want %v", got, want)
	}
}

// A check left out by -only says so in every language, in two lines of text.
func TestSkippedRenders(t *testing.T) {
	run := result.Run{Checks: []result.Check{skipped("tcp-load", 3, result.Path{Node: "192.0.2.10:7443", Family: "ipv4", Protocol: "tcp"}, "check.not_selected")}}
	for _, lang := range i18n.Supported {
		cat, err := i18n.Load(lang)
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if err := report.Text(&b, run, cat); err != nil {
			t.Fatal(err)
		}
		if out := b.String(); strings.Contains(out, "check.not_selected") || strings.Count(out, "\n") != 2 || !strings.Contains(out, "SKIP") {
			t.Errorf("%s:\n%s", lang, out)
		}
	}
}

// A check left out by -only has the JSON shape of one that ran: objects and
// arrays where a run has them, never null.
func TestSkippedKeepsTheJSONShape(t *testing.T) {
	b, err := json.Marshal(skipped("tcp-load", 3, result.Path{Node: "192.0.2.10:7443", Family: "ipv4", Protocol: "tcp"}, "check.not_selected"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"params":{}`, `"metrics":[]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%s lacks %s", b, want)
		}
	}
}
