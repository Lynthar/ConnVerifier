package runner

import (
	"errors"
	"strings"
	"testing"
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
